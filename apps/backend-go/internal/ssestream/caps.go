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

// AnonymousUser is the user id for self-host chat (CHAT_REQUIRE_AUTH off, no user). Python has
// no per-user chat cap, so anonymous callers skip the per-user limit (the global pool still
// bounds them) instead of sharing one capped bucket.
const AnonymousUser int32 = 0

// Caps limits concurrent chat streams per user and process-wide.
type Caps struct {
	sem     chan struct{}
	perUser int
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
	return &Caps{sem: sem, perUser: max(perUser, 1), byUser: map[int32]int{}}
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
	c.byUser[userID]++ // anonymous slots are counted (Active) but never refused
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
