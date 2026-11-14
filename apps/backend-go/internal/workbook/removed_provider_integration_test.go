//go:build integration

package workbook_test

import (
	"context"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// removedRunner serves only "ok"; every other name is neither a Go provider nor
// a manifest, like a provider that was removed from the build (holehe).
type removedRunner struct{ calls []string }

func (r *removedRunner) Serves(name string) bool { return name == "ok" }

func (r *removedRunner) Run(_ context.Context, _, name string, _ providers.Input, _ time.Duration) (providers.Output, error) {
	r.calls = append(r.calls, name)
	if name != "ok" {
		return providers.Output{}, provcall.ErrUnavailable
	}
	f := providers.NewObj()
	f.Set(target, "7")
	return providers.Output{Extra: f, Confidence: 0.9}, nil
}

// A stored waterfall that still names a removed provider must skip it and let
// the later providers run, not fail the run or stop the waterfall.
func TestStoredChainNamingRemovedProviderIsSkipped(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-gone", "["+colJSON("c1", "holehe", "ok")+"]", 1, 0)
	raw := mutate(payloadFor(wsA, "wb-gone", map[string]float64{"holehe": 0, "ok": 0}, nil), func(p map[string]any) {
		delete(p["go"].(map[string]any)["manifests"].(map[string]any), "holehe")
	})
	r := &removedRunner{}
	d := e.deps(newSim(t, nil), nil)
	d.Runner = r
	res, err := workbook.Run(e.ctx, d, e.claim(wsA, raw))
	if err != nil || res.Completed != 1 || res.Errors != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(r.calls) != 1 || r.calls[0] != "ok" {
		t.Fatalf("the removed provider must never be called: %v", r.calls)
	}
	if v := e.str(wsA, `SELECT value FROM workbook_enrichments WHERE column_id='c1'`); v != "7" {
		t.Fatalf("value %q", v)
	}
}
