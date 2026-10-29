package api

import (
	"context"
	"net/http"
	"sync"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// byteBudget is a weighted semaphore over upload bytes in flight. Memory of an
// import scales with its body size (about ten times once decoded), not with the
// request count, so the unit is bytes.
type byteBudget struct {
	mu    sync.Mutex
	avail int64
	cap   int64
	wake  chan struct{}
}

func newByteBudget(capBytes int64) *byteBudget {
	return &byteBudget{avail: capBytes, cap: capBytes, wake: make(chan struct{})}
}

// acquire takes n bytes (clamped to the whole budget, so any single upload
// fits), waiting until they are free or ctx ends. The returned func releases
// them (idempotent).
func (b *byteBudget) acquire(ctx context.Context, n int64) (release func(), ok bool) {
	if n < 1 {
		n = 1
	}
	if n > b.cap {
		n = b.cap
	}
	for {
		b.mu.Lock()
		if b.avail >= n {
			b.avail -= n
			b.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					b.mu.Lock()
					b.avail += n
					close(b.wake)
					b.wake = make(chan struct{})
					b.mu.Unlock()
				})
			}, true
		}
		wake := b.wake
		b.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return nil, false
		}
	}
}

func (b *byteBudget) available() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.avail
}

var (
	sharedMu      sync.Mutex
	sharedBudgets = map[int]*byteBudget{}
)

// sharedBudget returns the process-wide upload budget for the configured size
// (IMPORT_MEMORY_BUDGET_MB, default 512): import, add rows and workbook
// create/update/from-jobs all draw on it so concurrent buffered bodies cannot
// multiply past it.
func sharedBudget(mb int) *byteBudget {
	if mb <= 0 {
		mb = 512
	}
	sharedMu.Lock()
	defer sharedMu.Unlock()
	b, ok := sharedBudgets[mb]
	if !ok {
		b = newByteBudget(int64(mb) << 20)
		sharedBudgets[mb] = b
	}
	return b
}

// withBodyBudget reserves the declared body size (maxBody when unknown) from b
// before the handler reads the body; 503 + Retry-After when it stays full.
// Place it after authentication and the role gate (it wraps the handler).
func withBodyBudget(b *byteBudget, maxBody int64, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		weight := r.ContentLength
		if weight < 0 {
			weight = maxBody
		}
		ctx, cancel := context.WithTimeout(r.Context(), importBudgetWait)
		release, ok := b.acquire(ctx, weight)
		cancel()
		if !ok {
			if r.Context().Err() != nil {
				return
			}
			httpx.Detail(w, http.StatusServiceUnavailable, "Too many uploads in progress", httpx.Header{Key: "Retry-After", Value: "5"})
			return
		}
		defer release()
		h(w, r)
	}
}
