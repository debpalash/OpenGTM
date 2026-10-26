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
