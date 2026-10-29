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
	if rp["lock_timeout"] != "30s" || rp["statement_timeout"] != "60s" || rp["TimeZone"] != "UTC" || pc.MaxConns != 3 {
		t.Fatalf("%v max=%d", rp, pc.MaxConns)
	}
}

func TestRunnerDispatchesByType(t *testing.T) {
	var got []string
	mk := func(name string) runner {
		return func(context.Context, *queue.Claim) error { got = append(got, name); return nil }
	}
	rn := dispatch(map[string]runner{"run_workbook": mk("run"), "refresh_workbook": mk("refresh"), "signal_scan": mk("scan")})
	for _, typ := range []string{"signal_scan", "run_workbook", "refresh_workbook"} {
		if err := rn(context.Background(), &queue.Claim{Type: typ}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(got, ",") != "scan,run,refresh" {
		t.Fatalf("got %v", got)
	}
	err := rn(context.Background(), &queue.Claim{Type: "mystery"})
	if err == nil || !queue.IsPermanent(err) || !strings.Contains(err.Error(), "mystery") {
		t.Fatalf("unknown type: %v", err)
	}
}

func TestPerTypeTimeouts(t *testing.T) {
	cfg := config.Config{JobTimeout: 1234 * time.Second}
	for typ, want := range map[string]time.Duration{
		"run_workbook": 1234 * time.Second, "refresh_workbook": 900 * time.Second, "signal_scan": 300 * time.Second,
		"ambitionbox_import": 900 * time.Second,
	} {
		if got := jobLimit(cfg, typ); got != want {
			t.Errorf("%s: %s want %s", typ, got, want)
		}
	}
}

func TestSignalCapsFromConfig(t *testing.T) {
	c := signalCaps(config.ExecutorSettings{SignalHotLeads: 1, SignalWarmLeads: 2, SignalPerWorkspace: 3, SignalGlobal: 0})
	if c.HotLeads != 1 || c.WarmLeads != 2 || c.PerWorkspace != 3 || c.Global != 0 {
		t.Fatalf("%+v", c)
	}
}

func TestPanicInHandlerFailsJobNotProcess(t *testing.T) {
	rn := dispatch(map[string]runner{"refresh_workbook": func(context.Context, *queue.Claim) error { panic("kaboom") }})
	err := runWithDeadline(context.Background(), telemetry.NewLogger(io.Discard), rn, &queue.Claim{Type: "refresh_workbook"}, time.Minute)
	if err == nil || !strings.HasPrefix(err.Error(), "panic: kaboom") {
		t.Fatalf("got %v", err)
	}
}
