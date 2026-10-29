package collect

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
)

type existErrStore struct{ fakeStore }

func (*existErrStore) ExistingCompanies(context.Context) (map[string]bool, error) {
	return nil, errors.New("canceling statement due to statement timeout")
}

// L1: a failed read of the workspace's companies is surfaced on the dedup stage and in progress,
// not swallowed (the run continues: re-found companies no longer overwrite user fields).
func TestDedupReadErrorSurfaced(t *testing.T) {
	var v struct {
		Jobs []jobVector `json:"jobs"`
		Maps map[string]struct {
			Query, City string
			Places      []browserclient.MapPlace
		} `json:"maps"`
	}
	load(t, "job.json", &v)
	c := v.Jobs[0]
	h := newHarness(t, c.Calls, c.Spec.LLM == "down", fakeSources{enabled: c.Spec.Enabled}, &fakeBrowser{w: loadWorld(t), places: v.Maps})
	led := newFakeLedger(0)
	st := &existErrStore{}
	if err := h.runner.Process(context.Background(), Job{ID: "job1", Query: c.Spec.Query, WorkspaceID: "ws1",
		Ledger: led, Store: st, Emit: h.em}); err != nil {
		t.Fatalf("run: %v", err)
	}
	var dedup map[string]any
	for _, s := range led.stages {
		if s["stage"] == "dedup" {
			dedup = s
		}
	}
	det, _ := dedup["details"].(map[string]any)
	if det["existing_error"] != "existing companies could not be read; cross-job dedup skipped" {
		t.Errorf("dedup details %v", det)
	}
	if strings.Contains(pyDumps(det), "statement timeout") {
		t.Errorf("raw database error in stage details: %v", det)
	}
	warned := false
	for _, e := range h.em.events {
		if e["stage"] == "dedup" && strings.Contains(e["message"].(string), "cross-job dedup skipped") {
			warned = true
		}
	}
	if !warned {
		t.Error("no progress warning for the failed dedup read")
	}
}
