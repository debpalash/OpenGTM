package pausectx

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestExpiresWithDeadlineExceeded(t *testing.T) {
	ctx, cancel := WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("did not expire")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("err %v", ctx.Err())
	}
}

func TestPausedTimeDoesNotCount(t *testing.T) {
	ctx, cancel := WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	resume := Pause(ctx)
	time.Sleep(150 * time.Millisecond) // longer than the whole timeout
	if ctx.Err() != nil {
		t.Fatalf("expired while paused: %v", ctx.Err())
	}
	resume()
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("err %v", ctx.Err())
	}
}

func TestBudgetPreservedAcrossPause(t *testing.T) {
	ctx, cancel := WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	time.Sleep(50 * time.Millisecond)
	resume := Pause(ctx)
	time.Sleep(300 * time.Millisecond)
	resume()
	start := time.Now()
	<-ctx.Done()
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Fatalf("only %v of the budget was left after the pause", d)
	}
}

func TestParentCancelWinsWhilePaused(t *testing.T) {
	parent, pcancel := context.WithCancel(context.Background())
	ctx, cancel := WithTimeout(parent, time.Minute)
	defer cancel()
	Pause(ctx)
	pcancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("parent cancel ignored")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("err %v", ctx.Err())
	}
}

func TestPauseWithoutClockIsNoop(t *testing.T) {
	Pause(context.Background())() // must not panic
}

func TestNestedPauseResume(t *testing.T) {
	ctx, cancel := WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	r1, r2 := Pause(ctx), Pause(ctx)
	time.Sleep(80 * time.Millisecond)
	r1()
	time.Sleep(80 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatalf("expired while one pause was still open: %v", ctx.Err())
	}
	r2()
	<-ctx.Done()
}
