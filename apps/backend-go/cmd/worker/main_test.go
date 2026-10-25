package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
)

func TestPanicBecomesFailedJobErrorAndProcessSurvives(t *testing.T) {
	var buf strings.Builder
	log := telemetry.NewLogger(&buf)
	long := strings.Repeat("x", 500)
	for name, rn := range map[string]runner{
		"string": func(context.Context, *queue.Claim) error { panic("boom") },
		"error":  func(context.Context, *queue.Claim) error { panic(errors.New("kaboom")) },
		"long":   func(context.Context, *queue.Claim) error { panic(long) },
	} {
		err := runWithDeadline(context.Background(), log, rn, &queue.Claim{}, time.Minute)
		if err == nil || !strings.HasPrefix(err.Error(), "panic: ") || len([]rune(err.Error())) > 200 {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(err.Error(), "goroutine") {
			t.Fatalf("%s: stack leaked into the job error: %v", name, err)
		}
	}
	if !strings.Contains(buf.String(), "job panicked") {
		t.Fatal("stack was not logged")
	}
}

func TestDeadlineCancelsJobContext(t *testing.T) {
	var saw error
	rn := func(ctx context.Context, _ *queue.Claim) error {
		<-ctx.Done()
		saw = ctx.Err()
		return ctx.Err()
	}
	start := time.Now()
	err := runWithDeadline(context.Background(), telemetry.NewLogger(io.Discard), rn, &queue.Claim{}, 50*time.Millisecond)
	if !errors.Is(saw, context.DeadlineExceeded) || err == nil || !strings.HasPrefix(err.Error(), "job timed out after") {
		t.Fatalf("saw=%v err=%v", saw, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("deadline did not fire promptly")
	}
}

func TestParentCancelIsNotATimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rn := func(ctx context.Context, _ *queue.Claim) error { cancel(); <-ctx.Done(); return ctx.Err() }
	err := runWithDeadline(ctx, telemetry.NewLogger(io.Discard), rn, &queue.Claim{}, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestPoolConfigCarriesSessionTimeouts(t *testing.T) {
	pc, err := poolConfig(config.Config{DatabaseURL: "postgres://u:p@h:5432/d", DBMaxConns: 3})
	if err != nil {
		t.Fatal(err)
	}
	rp := pc.ConnConfig.RuntimeParams
	if rp["lock_timeout"] != "30s" || rp["statement_timeout"] != "60s" || pc.MaxConns != 3 {
		t.Fatalf("%v max=%d", rp, pc.MaxConns)
	}
}
