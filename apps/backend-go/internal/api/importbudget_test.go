package api

import (
	"context"
	"testing"
	"time"
)

func TestByteBudget(t *testing.T) {
	b := newByteBudget(100)
	ctx := context.Background()
	r1, ok := b.acquire(ctx, 60)
	if !ok || b.available() != 40 {
		t.Fatal("first acquire")
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	if _, ok := b.acquire(short, 50); ok {
		t.Fatal("over-budget acquire succeeded")
	}
	cancel()
	done := make(chan func(), 1)
	go func() { r, _ := b.acquire(ctx, 50); done <- r }()
	time.Sleep(20 * time.Millisecond)
	r1()
	r1() // idempotent
	select {
	case r2 := <-done:
		if b.available() != 50 {
			t.Fatalf("available %d", b.available())
		}
		r2()
	case <-time.After(2 * time.Second):
		t.Fatal("waiter was not woken by release")
	}
	if b.available() != 100 {
		t.Fatalf("budget leaked: %d", b.available())
	}
	// a weight above the budget is clamped, so one max upload always fits
	r, ok := b.acquire(ctx, 1<<40)
	if !ok || b.available() != 0 {
		t.Fatal("clamped acquire")
	}
	r()
}
