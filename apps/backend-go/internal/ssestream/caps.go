package ssestream

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// CapError is a refused acquisition; Status and Detail are what the route answers.
type CapError struct {
	Status int
	Detail string
}

func (e *CapError) Error() string { return e.Detail }

// AnonymousUser is the user id for self-host chat (CHAT_REQUIRE_AUTH off, no user). Anonymous
// callers share one separate counter (DefaultAnonStreams, COPILOT_MAX_ANON_STREAMS), so they
// cannot exhaust the global pool nor the per-user budget of real users.
const AnonymousUser int32 = 0

// DefaultAnonStreams is the default cap on concurrent anonymous chat streams.
const DefaultAnonStreams = 4

// Caps limits concurrent chat streams per user and process-wide.
type Caps struct {
	sem     chan struct{}
	perUser int
	anon    int
	mu      sync.Mutex
	byUser  map[int32]int
}

// NewCaps creates caps with its own global pool (SSE_MAX_STREAMS semantics) and a
// per-user limit (COPILOT_MAX_STREAMS_PER_USER).
func NewCaps(global, perUser int) *Caps {
	return NewCapsShared(make(chan struct{}, max(global, 1)), perUser)
}

// NewCapsShared uses an existing global semaphore, so chat and /api/events can draw on one
// SSE_MAX_STREAMS pool.
func NewCapsShared(sem chan struct{}, perUser int) *Caps {
	return &Caps{sem: sem, perUser: max(perUser, 1), anon: DefaultAnonStreams, byUser: map[int32]int{}}
}

// SetAnonLimit sets the cap on concurrent anonymous streams (n < 1 is ignored).
func (c *Caps) SetAnonLimit(n int) {
	if n < 1 {
		return
	}
	c.mu.Lock()
	c.anon = n
	c.mu.Unlock()
}

// Active is the number of held per-user slots.
func (c *Caps) Active() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.byUser {
		n += v
	}
	return n
}

// Acquire takes a per-user then a global slot. The workspace is accepted for symmetry with the
// events route; chat is capped per user. release is idempotent. Callers must
// `rel, err := Acquire(...)` and, on success, `defer rel()` immediately so a panic or
// cancelled request cannot leak the slot.
func (c *Caps) Acquire(ctx context.Context, workspaceID string, userID int32) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	anon := userID == AnonymousUser
	c.mu.Lock()
	if anon && c.byUser[userID] >= c.anon {
		c.mu.Unlock()
		return nil, &CapError{http.StatusTooManyRequests, "Too many active chat streams"}
	}
	if !anon && c.byUser[userID] >= c.perUser {
		c.mu.Unlock()
		return nil, &CapError{http.StatusTooManyRequests, "Too many active chat streams"}
	}
	select {
	case c.sem <- struct{}{}:
	default:
		c.mu.Unlock()
		return nil, &CapError{http.StatusServiceUnavailable, "Too many event streams"}
	}
	c.byUser[userID]++
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.byUser[userID]--; c.byUser[userID] <= 0 {
				delete(c.byUser, userID)
			}
			<-c.sem
		})
	}, nil
}

// KeepaliveLoop writes ": keepalive" every `every` until ctx ends, done closes, or a write
// fails (client gone).
func KeepaliveLoop(ctx context.Context, every time.Duration, w *Writer, done <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-t.C:
			if w.Keepalive() != nil {
				return
			}
		}
	}
}
