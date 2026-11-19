// Package pausectx is a timeout context whose clock can be paused. A caller that has to queue for a shared
// resource (the browser service's slots) pauses the clock of the provider timeout it runs under, so time spent
// waiting for a slot is not counted as the provider being slow. The parent's cancellation still ends the
// context at once, paused or not.
package pausectx

import (
	"context"
	"sync"
	"time"
)

type key struct{}

type clock struct {
	parent    context.Context
	done      chan struct{}
	mu        sync.Mutex
	err       error
	timer     *time.Timer
	remaining time.Duration
	started   time.Time
	paused    int
}

// WithTimeout is context.WithTimeout with a pausable clock. Err is context.DeadlineExceeded when the
// (unpaused) time ran out, otherwise the parent's error or context.Canceled.
func WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	c := &clock{parent: parent, done: make(chan struct{}), remaining: d, started: time.Now()}
	c.mu.Lock()
	c.timer = time.AfterFunc(d, func() { c.finish(context.DeadlineExceeded) })
	c.mu.Unlock()
	stop := context.AfterFunc(parent, func() { c.finish(parent.Err()) })
	return c, func() { stop(); c.finish(context.Canceled) }
}

func (c *clock) finish(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = err
	c.timer.Stop()
	close(c.done)
}

func (c *clock) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *clock) Done() <-chan struct{}       { return c.done }
func (c *clock) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}
func (c *clock) Value(k any) any {
	if _, ok := k.(key); ok {
		return c
	}
	return c.parent.Value(k)
}

// Pause stops the nearest pausable clock in ctx and returns the function that restarts it with the time that
// was left. Pauses nest. With no pausable clock in ctx it does nothing.
func Pause(ctx context.Context) (resume func()) {
	c, _ := ctx.Value(key{}).(*clock)
	if c == nil {
		return func() {}
	}
	c.mu.Lock()
	if c.err == nil {
		if c.paused == 0 {
			c.timer.Stop()
			c.remaining -= time.Since(c.started)
		}
		c.paused++
	}
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.err != nil || c.paused == 0 {
				return
			}
			c.paused--
			if c.paused == 0 {
				c.started = time.Now()
				if c.remaining <= 0 {
					c.remaining = time.Millisecond
				}
				c.timer.Reset(c.remaining)
			}
		})
	}
}
