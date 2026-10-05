//go:build linux

package process

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests need /proc to observe the process tree, hence linux only.

func TestTimeoutKillsTheWholeProcessTree(t *testing.T) {
	s := newSup(t, Options{CancelGrace: 200 * time.Millisecond})
	p := pspec{timeout: 1.5}.build(t)
	pidfile := filepath.Join(t.TempDir(), "pids.json")

	done := make(chan error, 1)
	go func() {
		_, err := run(t, s, p, map[string]any{"mode": "tree", "pidfile": pidfile}, nil)
		done <- err
	}()
	pids := readTree(t, pidfile)
	pids.assertAlive(t) // control: the processes exist before the timeout

	start := time.Now()
	err := <-done
	pe := wantError(t, err, CodeTimeout, true)
	if !strings.Contains(pe.Message, "limits.timeout_seconds") {
		t.Fatalf("message: %s", pe.Message)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("timeout took %s to be enforced", d)
	}
	// Run has returned: nothing it started may still be running, including
	// the child that called setsid and the orphaned double fork.
	pids.assertGone(t, 2*time.Second)
	if st := s.Stats(); st.TimedOut != 1 || st.Running != 0 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestCancelKillsTheWholeProcessTreeEvenWhenSignalsAreIgnored(t *testing.T) {
	s := newSup(t, Options{CancelGrace: 300 * time.Millisecond})
	p := pspec{timeout: 60}.build(t)
	pidfile := filepath.Join(t.TempDir(), "pids.json")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.Run(ctx, p, Request{Inputs: map[string]any{"mode": "ignore_cancel", "pidfile": pidfile}})
		done <- err
	}()
	pids := readTree(t, pidfile)
	pids.assertAlive(t)

	start := time.Now()
	cancel()
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("want a cancellation error, got %v", err)
	}
	if _, ok := err.(*Error); ok {
		t.Fatalf("cancellation must not look like a plugin failure: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("cancel took %s", d)
	}
	pids.assertGone(t, 2*time.Second)
	if st := s.Stats(); st.Cancelled != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestCooperativeCancelLetsThePluginStopItself(t *testing.T) {
	s := newSup(t, Options{CancelGrace: 3 * time.Second})
	p := pspec{timeout: 60}.build(t)
	ctx, cancel := context.WithCancel(context.Background())
	var cancelledAt time.Time
	log := s.opts.Logger.(*captureLog)
	go func() {
		for !strings.Contains(log.String(), "ready") {
			time.Sleep(10 * time.Millisecond)
		}
		cancelledAt = time.Now()
		cancel()
	}()
	_, err := s.Run(ctx, p, Request{Inputs: map[string]any{"mode": "cooperative"}})
	if err == nil || !strings.Contains(err.Error(), "cancel") {
		t.Fatalf("err: %v", err)
	}
	// The plugin exited on the cancel frame; no need to wait out the grace.
	if d := time.Since(cancelledAt); d > 2*time.Second {
		t.Fatalf("took %s despite a cooperative plugin", d)
	}
}

func TestStraysAreKilledAfterNormalCompletion(t *testing.T) {
	s := newSup(t, Options{})
	p := pspec{}.build(t)
	pidfile := filepath.Join(t.TempDir(), "pids.json")
	mustRun(t, s, p, map[string]any{"mode": "tree_then_ok", "pidfile": pidfile}, nil)
	pids := readTree(t, pidfile)
	// The plugin succeeded but left three background processes behind.
	pids.assertGone(t, 2*time.Second)
}

func TestPluginThatHangsAfterItsResultIsKilled(t *testing.T) {
	s := newSup(t, Options{ExitGrace: 300 * time.Millisecond})
	p := pspec{}.build(t)
	start := time.Now()
	out := mustRun(t, s, p, map[string]any{"mode": "hang_after_result"}, nil)
	if len(out.Records) != 1 {
		t.Fatalf("result lost: %+v", out)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s", d)
	}
}

func TestCloseKillsRunningPlugins(t *testing.T) {
	s := newSup(t, Options{})
	p := pspec{timeout: 60}.build(t)
	pidfile := filepath.Join(t.TempDir(), "pids.json")
	done := make(chan error, 1)
	go func() {
		_, err := run(t, s, p, map[string]any{"mode": "tree", "pidfile": pidfile}, nil)
		done <- err
	}()
	pids := readTree(t, pidfile)
	s.Close()
	if err := <-done; err == nil {
		t.Fatal("run survived Close")
	}
	pids.assertGone(t, 2*time.Second)
	if _, err := run(t, s, p, map[string]any{}, nil); err == nil {
		t.Fatal("closed supervisor accepted a run")
	}
}
