//go:build integration

package providerrun_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

type targetProv struct {
	fakeProv
	providers.TargetSite
}

func blockUntilDone(ctx context.Context, _ *http.Client, _ func() (string, error), _ providers.Input) (providers.Output, error) {
	<-ctx.Done()
	return providers.Output{}, ctx.Err()
}

// A provider that fetches the lead's own website times out because of that website: no number of timeouts benches
// it for the leads that follow.
func TestTargetSiteProviderTimeoutsNeverCoolDown(t *testing.T) {
	name := uniq("t18_target")
	providers.Register(&targetProv{fakeProv: fakeProv{name, blockUntilDone}})
	h := newHarness(t, nil)
	for i := 0; i < 6; i++ {
		out, err := h.r.Run(context.Background(), "ws1", name, providers.Input{Field: "description"}, 30*time.Millisecond)
		if err != nil || out.Error != "timeout" {
			t.Fatalf("run %d: %+v %v", i, out, err)
		}
	}
	s, ok := h.stat(t, name, "description")
	if !ok || s.attempts != 6 || s.cooldownSecondsAhead != -1 {
		t.Fatalf("ledger after six target-site timeouts %+v ok=%v (want 6 attempts, no cooldown)", s, ok)
	}
}

// Twelve rows launched together that all time out against an upstream are one event, not twelve.
func TestConcurrentUpstreamTimeoutsAreOneEvent(t *testing.T) {
	name := uniq("t18_burst")
	providers.Register(&fakeProv{name, blockUntilDone})
	h := newHarness(t, func(d *providerrun.Deps) { d.Workers = 12 })
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if out, err := h.r.Run(context.Background(), "ws1", name, providers.Input{Field: "description"}, 400*time.Millisecond); err != nil || out.Error != "timeout" {
				t.Errorf("%+v %v", out, err)
			}
		}()
	}
	wg.Wait()
	s, ok := h.stat(t, name, "description")
	if !ok || s.attempts != 12 || s.cooldownSecondsAhead != -1 {
		t.Fatalf("ledger after one burst of 12 timeouts %+v ok=%v (want 12 attempts, no cooldown)", s, ok)
	}
	// Two more successive rounds are three events: now it is benched.
	for round := 0; round < 2; round++ {
		if out, err := h.r.Run(context.Background(), "ws1", name, providers.Input{Field: "description"}, 30*time.Millisecond); err != nil || out.Error != "timeout" {
			t.Fatalf("round %d: %+v %v", round, out, err)
		}
	}
	if s, _ = h.stat(t, name, "description"); s.cooldownSecondsAhead < 150 {
		t.Fatalf("after three successive rounds of timeouts %+v (want a ~180 s cooldown)", s)
	}
}
