package ssestream

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestCapsPerUserAndGlobal(t *testing.T) {
	c := NewCaps(3, 2)
	ctx := context.Background()
	r1, err := c.Acquire(ctx, "ws", 1)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := c.Acquire(ctx, "ws", 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Acquire(ctx, "ws", 1)
	var ce *CapError
	if !errors.As(err, &ce) || ce.Status != http.StatusTooManyRequests || ce.Detail != "Too many active chat streams" {
		t.Fatalf("per-user: %v", err)
	}
	r3, err := c.Acquire(ctx, "ws", 2) // other user fits (global 3)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Acquire(ctx, "ws", 3) // global exhausted
	if !errors.As(err, &ce) || ce.Status != http.StatusServiceUnavailable || ce.Detail != "Too many event streams" {
		t.Fatalf("global: %v", err)
	}
	r1()
	r4, err := c.Acquire(ctx, "ws", 3)
	if err != nil {
		t.Fatalf("slot not freed: %v", err)
	}
	r2()
	r3()
	r4()
	if c.Active() != 0 {
		t.Fatalf("leak: %d", c.Active())
	}
}

func TestReleaseIdempotent(t *testing.T) {
	c := NewCaps(2, 1)
	r, err := c.Acquire(context.Background(), "ws", 1)
	if err != nil {
		t.Fatal(err)
	}
	r()
	r()
	r()
	// a double release must not free a slot held by someone else
	ra, _ := c.Acquire(context.Background(), "ws", 1)
	r()
	if _, err := c.Acquire(context.Background(), "ws", 1); err == nil {
		t.Fatal("double release freed another holder's slot")
	}
	ra()
	if c.Active() != 0 {
		t.Fatalf("active %d", c.Active())
	}
}

func TestAcquireCancelledContext(t *testing.T) {
	c := NewCaps(2, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Acquire(ctx, "ws", 1); err == nil {
		t.Fatal("want ctx error")
	}
	if c.Active() != 0 {
		t.Fatal("slot taken")
	}
}

func TestSharedGlobalSemaphore(t *testing.T) {
	sem := make(chan struct{}, 1)
	c := NewCapsShared(sem, 5)
	r, err := c.Acquire(context.Background(), "ws", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(sem) != 1 {
		t.Fatal("shared sem not used")
	}
	r()
	if len(sem) != 0 {
		t.Fatal("shared sem not released")
	}
}

// Contract for callers (Tasks 13/17): the slot is held until release runs, so the handler
// must `rel, err := Acquire(...)`; on success `defer rel()` immediately. This documents and
// checks that a panicking handler that defers release does not leak.
func TestAcquireThenDeferReleasesOnPanic(t *testing.T) {
	c := NewCaps(2, 1)
	func() {
		defer func() { _ = recover() }()
		rel, err := c.Acquire(context.Background(), "ws", 1)
		if err != nil {
			t.Fatal(err)
		}
		defer rel()
		panic("boom")
	}()
	if c.Active() != 0 {
		t.Fatalf("leak after panic: %d", c.Active())
	}
}

// Self-host (CHAT_REQUIRE_AUTH off) has no user id: anonymous callers share one separate counter
// (default 4, SetAnonLimit) that does not eat the per-user budget; the 5th concurrent anonymous stream gets 429.
func TestAnonymousStreamsAreCappedSeparately(t *testing.T) {
	c := NewCaps(20, 1)
	var rels []func()
	for i := 0; i < 4; i++ {
		r, err := c.Acquire(context.Background(), "ws", AnonymousUser)
		if err != nil {
			t.Fatalf("anon %d: %v", i, err)
		}
		rels = append(rels, r)
	}
	_, err := c.Acquire(context.Background(), "ws", AnonymousUser)
	var ce *CapError
	if !errors.As(err, &ce) || ce.Status != http.StatusTooManyRequests {
		t.Fatalf("5th anonymous acquire: %v", err)
	}
	if r, err := c.Acquire(context.Background(), "ws", 7); err != nil { // real users unaffected
		t.Fatalf("user blocked by anon load: %v", err)
	} else {
		r()
	}
	rels[0]()
	rels[0]()
	r, err := c.Acquire(context.Background(), "ws", AnonymousUser)
	if err != nil {
		t.Fatalf("slot not freed: %v", err)
	}
	rels = append(rels[1:], r)
	c.SetAnonLimit(2)
	if _, err := c.Acquire(context.Background(), "ws", AnonymousUser); err == nil {
		t.Fatal("SetAnonLimit(2) not applied")
	}
	for _, r := range rels {
		r()
	}
	if c.Active() != 0 {
		t.Fatalf("leak %d", c.Active())
	}
	small := NewCaps(2, 1)
	r1, _ := small.Acquire(context.Background(), "ws", AnonymousUser)
	r2, _ := small.Acquire(context.Background(), "ws", AnonymousUser)
	if _, err := small.Acquire(context.Background(), "ws", AnonymousUser); err == nil {
		t.Fatal("global cap must still bound anonymous")
	}
	r1()
	r2()
}
