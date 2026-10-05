package authz

import (
	"fmt"
	"sync"
	"time"
)

// The two fail-fast refusals. Both wrap ErrUnavailable, so callers that only
// test errors.Is(err, ErrUnavailable) keep working, and WriteError renders
// them as 503 with Retry-After rather than the generic 502.
var (
	// ErrCircuitOpen means recent calls to the legacy API failed repeatedly, so
	// new calls are refused without touching it until the cool-down passes.
	ErrCircuitOpen = fmt.Errorf("%w: circuit open after repeated failures", ErrUnavailable)
	// ErrOverloaded means the bounded number of in-flight authorization calls
	// is already running; the request is shed instead of queueing behind them.
	ErrOverloaded = fmt.Errorf("%w: too many authorization calls in flight", ErrUnavailable)
)

// breaker is a consecutive-failure circuit breaker.
//
// Closed: calls pass; a success resets the failure count; threshold
// consecutive failures open the circuit. Open: calls are refused until
// cooldown has passed. Then half-open: exactly one probe call is admitted,
// and its outcome closes the circuit or re-opens it for another cool-down.
//
// Only failures that say the legacy API itself is unhealthy (timeouts,
// connection errors, 5xx, malformed answers) count. A 401/403/404 is a healthy
// answer about a caller, and our own load shedding is not a failure at all.
type breaker struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	mu       sync.Mutex
	failures int
	openedAt time.Time
	open     bool
	probing  bool
}

// allow reports whether a call may proceed. A caller that is allowed must
// report its outcome with success or failure exactly once, or release if the
// call was never made.
func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return true
	}
	if b.probing || b.now().Sub(b.openedAt) < b.cooldown {
		return false
	}
	b.probing = true
	return true
}

func (b *breaker) success() {
	b.mu.Lock()
	b.failures, b.open, b.probing = 0, false, false
	b.mu.Unlock()
}

func (b *breaker) failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.probing { // the probe failed: back to a full cool-down
		b.probing, b.openedAt = false, b.now()
		return
	}
	b.failures++
	if !b.open && b.failures >= b.threshold {
		b.open, b.openedAt = true, b.now()
	}
}

// release gives back an admission that did not result in a call to the legacy
// API (shed by the concurrency bound), so a half-open probe slot is not lost.
func (b *breaker) release() {
	b.mu.Lock()
	b.probing = false
	b.mu.Unlock()
}

// isOpen is for tests and diagnostics.
func (b *breaker) isOpen() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.open
}

// call is one in-flight upstream lookup shared by every caller that asked for
// the same (token, workspace) while it ran.
type call struct {
	done chan struct{}
	ws   Workspace
	err  error
}
