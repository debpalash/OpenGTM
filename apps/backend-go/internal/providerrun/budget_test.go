//go:build integration

package providerrun_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

func sleepsThenFinds(d time.Duration) enrichFn {
	return func(ctx context.Context, _ *http.Client, _ func() (string, error), _ providers.Input) (providers.Output, error) {
		select {
		case <-time.After(d):
			return okOut("description", "found", 0.5), nil
		case <-ctx.Done():
			return providers.Output{}, ctx.Err()
		}
	}
}

// A provider that walks several of the lead's own pages needs more than the global provider timeout: it gets the
// target-site budget when that is larger, and the global timeout is not raised for anyone else.
func TestTargetSiteProviderGetsItsOwnTimeoutBudget(t *testing.T) {
	target, upstream := uniq("t18_budget_t"), uniq("t18_budget_u")
	providers.Register(&targetProv{fakeProv: fakeProv{target, sleepsThenFinds(250 * time.Millisecond)}})
	providers.Register(&fakeProv{upstream, sleepsThenFinds(250 * time.Millisecond)})
	h := newHarness(t, func(d *providerrun.Deps) { d.TargetSiteTimeout = 2 * time.Second })

	out, err := h.r.Run(context.Background(), "ws1", target, providers.Input{Field: "description"}, 100*time.Millisecond)
	if err != nil || out.Error != "" || out.Value != "found" {
		t.Fatalf("target-site provider with a 100 ms global timeout and a 2 s budget: %+v %v", out, err)
	}
	out, err = h.r.Run(context.Background(), "ws1", upstream, providers.Input{Field: "description"}, 100*time.Millisecond)
	if err != nil || out.Error != "timeout" {
		t.Fatalf("upstream provider must keep the global timeout: %+v %v", out, err)
	}
}

// A global timeout larger than the budget still wins: the budget is a floor for target-site providers.
func TestTargetSiteBudgetIsAFloorNotACap(t *testing.T) {
	target := uniq("t18_budget_floor")
	providers.Register(&targetProv{fakeProv: fakeProv{target, sleepsThenFinds(400 * time.Millisecond)}})
	h := newHarness(t, func(d *providerrun.Deps) { d.TargetSiteTimeout = 100 * time.Millisecond })
	out, err := h.r.Run(context.Background(), "ws1", target, providers.Input{Field: "description"}, 2*time.Second)
	if err != nil || out.Value != "found" {
		t.Fatalf("%+v %v", out, err)
	}
}
