//go:build linux

package process

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// livePluginProcesses counts running processes of rawplugin.py by reading
// /proc, independently of the supervisor's own accounting.
func livePluginProcesses() int {
	ents, _ := os.ReadDir("/proc")
	n := 0
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		cmd, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil || !bytes.Contains(cmd, []byte("rawplugin.py")) {
			continue
		}
		if procGone(mustAtoi(e.Name())) {
			continue
		}
		n++
	}
	return n
}

func mustAtoi(s string) int { n, _ := strconv.Atoi(s); return n }

// sample records the peak number of live plugin processes until stopped.
func sample(stop <-chan struct{}) <-chan int {
	peak := make(chan int, 1)
	go func() {
		max := 0
		for {
			if n := livePluginProcesses(); n > max {
				max = n
			}
			select {
			case <-stop:
				peak <- max
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	return peak
}

func TestConcurrencyIsBoundedUnderLoad(t *testing.T) {
	const cap, runs = 3, 24
	s := newSup(t, Options{MaxProcesses: cap, MaxPerPlugin: cap, QueueWait: time.Minute})
	p := pspec{}.build(t)

	stop := make(chan struct{})
	peak := sample(stop)
	var wg sync.WaitGroup
	var failed atomic.Int64
	start := time.Now()
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := run(t, s, p, map[string]any{"mode": "hold_slot", "seconds": 0.25}, nil); err != nil {
				t.Errorf("run: %v", err)
				failed.Add(1)
			}
		}()
	}
	// While saturated the pool reports what is waiting.
	time.Sleep(150 * time.Millisecond)
	if st := s.Stats(); st.Running != cap || st.Waiting != runs-cap {
		t.Errorf("under load: %+v (want %d running, %d waiting)", st, cap, runs-cap)
	}
	wg.Wait()
	close(stop)
	observed := <-peak

	if failed.Load() != 0 {
		t.Fatalf("%d runs failed", failed.Load())
	}
	if observed > cap {
		t.Fatalf("observed %d live plugin processes in /proc; the bound is %d", observed, cap)
	}
	if observed < cap {
		t.Fatalf("sampler saw at most %d processes; load was not saturating the pool (bound %d)", observed, cap)
	}
	st := s.Stats()
	if st.PeakRunning != cap || st.Succeeded != runs || st.Running != 0 || st.Waiting != 0 {
		t.Fatalf("stats: %+v", st)
	}
	// 24 runs of 0.25s through 3 slots cannot finish faster than 8 rounds.
	if d := time.Since(start); d < 8*250*time.Millisecond {
		t.Fatalf("finished in %s: runs were not serialized by the bound", d)
	}
}

func TestSaturatedPoolFailsFastWithARetryableError(t *testing.T) {
	s := newSup(t, Options{MaxProcesses: 1, QueueWait: 300 * time.Millisecond})
	p := pspec{timeout: 30}.build(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	holding := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(holding)
		_, _ = s.Run(ctx, p, Request{Inputs: map[string]any{"mode": "sleep", "seconds": 30}})
	}()
	<-holding
	for s.Stats().Running == 0 {
		time.Sleep(5 * time.Millisecond)
	}

	start := time.Now()
	_, err := run(t, s, p, map[string]any{"mode": "ok"}, nil)
	pe := wantError(t, err, CodeBusy, true)
	if !strings.Contains(pe.Message, "max 1 processes") {
		t.Fatalf("message: %s", pe.Message)
	}
	if d := time.Since(start); d < 250*time.Millisecond || d > 3*time.Second {
		t.Fatalf("waited %s, want about QueueWait", d)
	}
	if st := s.Stats(); st.Rejected != 1 || st.Waiting != 0 || st.Running != 1 {
		t.Fatalf("stats: %+v", st)
	}
	cancel()
	<-done
	// The slot comes back once the holder is gone.
	mustRun(t, s, p, map[string]any{"mode": "ok"}, nil)
}

func TestOnePluginCannotTakeEverySlot(t *testing.T) {
	s := newSup(t, Options{MaxProcesses: 4, MaxPerPlugin: 1, QueueWait: time.Minute})
	hog := pspec{name: "hog_plugin"}.build(t)
	other := pspec{name: "other_plugin"}.build(t)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mustRun(t, s, hog, map[string]any{"mode": "hold_slot", "seconds": 0.4}, nil)
		}()
	}
	time.Sleep(150 * time.Millisecond)
	// Three runs of "hog" are queued behind its single slot, yet another
	// plugin starts immediately.
	start := time.Now()
	mustRun(t, s, other, map[string]any{"mode": "hold_slot", "seconds": 0.01}, nil)
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Fatalf("other plugin waited %s behind hog_plugin", d)
	}
	if got := livePluginProcesses(); got > 1 {
		t.Fatalf("%d live processes for hog_plugin, limit 1", got)
	}
	wg.Wait()
}

func TestWaitingRunsAreReleasedWhenTheirContextEnds(t *testing.T) {
	s := newSup(t, Options{MaxProcesses: 1, QueueWait: time.Minute})
	p := pspec{timeout: 30}.build(t)
	hold, stopHold := context.WithCancel(context.Background())
	go func() { _, _ = s.Run(hold, p, Request{Inputs: map[string]any{"mode": "sleep", "seconds": 30}}) }()
	for s.Stats().Running == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := s.Run(ctx, p, Request{Inputs: map[string]any{"mode": "ok"}})
	if err == nil || ctx.Err() == nil {
		t.Fatalf("err: %v", err)
	}
	if st := s.Stats(); st.Waiting != 0 {
		t.Fatalf("a cancelled waiter leaked: %+v", st)
	}
	stopHold()
}
