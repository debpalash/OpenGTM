//go:build integration

package refresh_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/refresh"
)

// A claim workspace is strict: a payload naming another workspace is refused.
func TestRefreshRefusesPayloadWorkspaceMismatchWithClaim(t *testing.T) {
	pool, ctx := setupPool(t)
	seedStaleWorkbook(t, ctx, pool, wsA, "w1", refreshCols, `{"enabled":true,"interval":"daily"}`)
	err := refresh.Handle(ctx, deps(pool), claim(1000, wsA, map[string]any{"workbook_id": "w1", "workspace_id": wsB}))
	var perm *queue.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("err = %v, want a PermanentError", err)
	}
}

// Plan 15 cutover: the tenant comes from the claim only. A job without jobs.workspace_id (what Python's refresh.py
// wrote) is refused permanently with no side effects, whatever the payload names; the cutover runbook backfills
// such rows before the Python worker is gone.
func TestRefreshEmptyClaimRefused(t *testing.T) {
	for name, pl := range map[string]map[string]any{
		"payload ws matches":    {"workbook_id": "w1", "workspace_id": wsA},
		"payload ws omitted":    {"workbook_id": "w1"},
		"payload ws mismatched": {"workbook_id": "w1", "workspace_id": wsB},
		"missing workbook":      {"workbook_id": "nope", "workspace_id": wsA},
	} {
		t.Run(name, func(t *testing.T) {
			pool, ctx := setupPool(t)
			seedStaleWorkbook(t, ctx, pool, wsA, "w1", refreshCols, `{"enabled":true,"interval":"daily"}`)
			err := refresh.Handle(ctx, deps(pool), claim(1001, "", pl))
			var perm *queue.PermanentError
			if !errors.As(err, &perm) {
				t.Fatalf("err = %v, want a PermanentError", err)
			}
			for _, w := range []string{wsA, wsB} {
				if n := len(jobs(t, ctx, pool, w)); n != 0 {
					t.Errorf("ws %s: %d jobs enqueued", w, n)
				}
				if n, _ := activityCount(t, ctx, pool, w); n != 0 {
					t.Errorf("ws %s: %d activity rows", w, n)
				}
			}
		})
	}
}

// OnFailure follows the same rule: an empty notice workspace never re-seeds.
func TestRefreshFailureEmptyNoticeWorkspaceDoesNotReseed(t *testing.T) {
	pool, ctx := setupPool(t)
	seedStaleWorkbook(t, ctx, pool, wsA, "w1", refreshCols, `{"enabled":true,"interval":"daily"}`)
	for i, pl := range []map[string]any{
		{"workbook_id": "w1", "workspace_id": wsA},
		{"workbook_id": "w1", "workspace_id": wsB},
		{"workbook_id": "nope", "workspace_id": wsA},
	} {
		payload, _ := json.Marshal(pl)
		n := queue.FailureNotice{JobID: int64(4310 + i), JobType: "refresh_workbook", Payload: payload}
		if err := refresh.OnFailure(ctx, deps(pool), n); err != nil {
			t.Fatal(err)
		}
	}
	for _, w := range []string{wsA, wsB} {
		if got := len(ofType(jobs(t, ctx, pool, w), "refresh_workbook")); got != 0 {
			t.Errorf("ws %s: %d jobs re-seeded", w, got)
		}
	}
}

// With the claim's workspace the failure hook re-seeds the chain in it.
func TestRefreshFailureReseedsInTheClaimWorkspace(t *testing.T) {
	pool, ctx := setupPool(t)
	seedStaleWorkbook(t, ctx, pool, wsA, "w1", refreshCols, `{"enabled":true,"interval":"daily"}`)
	payload, _ := json.Marshal(map[string]any{"workbook_id": "w1", "workspace_id": wsA})
	n := queue.FailureNotice{JobID: 4300, JobType: "refresh_workbook", WorkspaceID: wsA, Payload: payload}
	if err := refresh.OnFailure(ctx, deps(pool), n); err != nil {
		t.Fatal(err)
	}
	js := ofType(jobs(t, ctx, pool, wsA), "refresh_workbook")
	if len(js) != 1 || js[0].Executor != "go" {
		t.Fatalf("want one re-seeded go job in the claim's workspace, got %+v", js)
	}
}
