package api

import (
	"context"
	"sync"
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
