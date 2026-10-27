package schedboot_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/schedboot"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestReconcileOnceIsolatesFailuresAndPanics(t *testing.T) {
	var ran []string
	subs := []schedboot.Subsystem{
		{Name: "a", Run: func(context.Context) (any, error) { ran = append(ran, "a"); return 3, nil }},
		{Name: "boom", Run: func(context.Context) (any, error) { ran = append(ran, "boom"); return nil, errors.New("db down") }},
		{Name: "panics", Run: func(context.Context) (any, error) { ran = append(ran, "panics"); panic("nil map") }},
		{Name: "z", Run: func(context.Context) (any, error) { ran = append(ran, "z"); return true, nil }},
	}
	res := schedboot.ReconcileOnce(context.Background(), subs, quiet, time.Second)
	if strings.Join(ran, ",") != "a,boom,panics,z" {
		t.Fatalf("every subsystem must run in order, ran %v", ran)
	}
	if res["a"] != 3 || res["z"] != true {
		t.Fatalf("results = %v", res)
	}
	if s, _ := res["boom"].(string); s != "error: db down" {
		t.Fatalf("boom = %v", res["boom"])
	}
	if s, _ := res["panics"].(string); !strings.HasPrefix(s, "error: panic: nil map") {
		t.Fatalf("panics = %v", res["panics"])
	}
}

func TestReconcileOnceTimeoutStopsOneSubsystemOnly(t *testing.T) {
	var after atomic.Bool
	subs := []schedboot.Subsystem{
		{Name: "hang", Run: func(ctx context.Context) (any, error) { <-ctx.Done(); return nil, ctx.Err() }},
		{Name: "next", Run: func(context.Context) (any, error) { after.Store(true); return 1, nil }},
	}
	res := schedboot.ReconcileOnce(context.Background(), subs, quiet, 50*time.Millisecond)
	if !after.Load() || res["next"] != 1 {
		t.Fatalf("the subsystem after a hung one must still run: %v", res)
	}
	if s, _ := res["hang"].(string); !strings.HasPrefix(s, "error:") {
		t.Fatalf("hang = %v", res["hang"])
	}
}

func TestReconcileInterval(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	for _, c := range []struct {
		in   string
		want time.Duration
	}{{"", 60 * time.Second}, {"90", 90 * time.Second}, {"3", 10 * time.Second}, {"-5", 10 * time.Second},
		{"abc", 60 * time.Second}, {" 120 ", 120 * time.Second}} {
		if got := schedboot.ReconcileInterval(env(c.in)); got != c.want {
			t.Errorf("SCHEDULER_RECONCILE_SECONDS=%q: %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSubsystemsOrderMatchesPython(t *testing.T) {
	var names []string
	for _, s := range (schedboot.Deps{}).Subsystems() {
		names = append(names, s.Name)
	}
	want := "signal_scan,automations,outreach,outreach_inbound,intent_poller,source_health,audiences,retention,playbooks"
	if strings.Join(names, ",") != want {
		t.Fatalf("subsystems = %v", names)
	}
}

func TestRunLoopReconcilesUntilCancelledAndOnlyWhenLeader(t *testing.T) {
	var runs atomic.Int32
	subs := []schedboot.Subsystem{{Name: "n", Run: func(context.Context) (any, error) { runs.Add(1); return 0, nil }}}
	leader := atomic.Bool{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- schedboot.Run(ctx, schedboot.RunConfig{Interval: 20 * time.Millisecond, SubsystemTimeout: time.Second, Log: quiet,
			Subsystems: subs, Hold: func(context.Context) (bool, error) { return leader.Load(), nil }})
	}()
	time.Sleep(120 * time.Millisecond)
	if runs.Load() != 0 {
		t.Fatalf("a standby scheduler must not reconcile (ran %d)", runs.Load())
	}
	leader.Store(true)
	time.Sleep(150 * time.Millisecond)
	if runs.Load() < 2 {
		t.Fatalf("leader should reconcile every interval, ran %d", runs.Load())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}

// M3: the lease is re-checked between subsystems; a pass stops as soon as it is lost.
func TestRunStopsPassWhenLeaseLostBetweenSubsystems(t *testing.T) {
	var ran []string
	var holds atomic.Int32
	subs := []schedboot.Subsystem{
		{Name: "a", Run: func(context.Context) (any, error) { ran = append(ran, "a"); return 0, nil }},
		{Name: "b", Run: func(context.Context) (any, error) { ran = append(ran, "b"); return 0, nil }},
		{Name: "c", Run: func(context.Context) (any, error) { ran = append(ran, "c"); return 0, nil }},
	}
	ctx, cancel := context.WithCancel(context.Background())
	hold := func(context.Context) (bool, error) {
		// 1st call: start of the pass; 2nd: before b (still held); 3rd: before c (lost) -> cancel after the pass
		n := holds.Add(1)
		if n >= 3 {
			cancel()
		}
		return n < 3, nil
	}
	_ = schedboot.Run(ctx, schedboot.RunConfig{Interval: time.Hour, SubsystemTimeout: time.Second, Log: quiet, Subsystems: subs, Hold: hold})
	if strings.Join(ran, ",") != "a,b" {
		t.Fatalf("a pass must stop when the lease is lost between subsystems, ran %v", ran)
	}
}

func TestRunStopsPassWhenLeaseCheckErrors(t *testing.T) {
	var ran []string
	var holds atomic.Int32
	subs := []schedboot.Subsystem{
		{Name: "a", Run: func(context.Context) (any, error) { ran = append(ran, "a"); return 0, nil }},
		{Name: "b", Run: func(context.Context) (any, error) { ran = append(ran, "b"); return 0, nil }},
	}
	ctx, cancel := context.WithCancel(context.Background())
	hold := func(context.Context) (bool, error) {
		if holds.Add(1) >= 2 {
			cancel()
			return false, errors.New("conn reset")
		}
		return true, nil
	}
	_ = schedboot.Run(ctx, schedboot.RunConfig{Interval: time.Hour, SubsystemTimeout: time.Second, Log: quiet, Subsystems: subs, Hold: hold})
	if strings.Join(ran, ",") != "a" {
		t.Fatalf("an unverifiable lease must stop the pass, ran %v", ran)
	}
}
