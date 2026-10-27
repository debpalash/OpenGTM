//go:build integration

package refresh_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/refresh"
)

// A non-empty claim workspace keeps the strict rule: a payload naming another workspace is refused.
func TestRefreshRefusesPayloadWorkspaceMismatchWithClaim(t *testing.T) {
	pool, ctx := setupPool(t)
	seedStaleWorkbook(t, ctx, pool, wsA, "w1", refreshCols, `{"enabled":true,"interval":"daily"}`)
	err := refresh.Handle(ctx, deps(pool), claim(1000, wsA, map[string]any{"workbook_id": "w1", "workspace_id": wsB}))
	var perm *queue.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("err = %v, want a PermanentError", err)
	}
}

// Python-enqueued refresh jobs (refresh.py _enqueue_next*) carry workspace_id only in the payload. With an empty
// claim workspace the tenant is the workbook's own workspace_id read from the database, never the payload's.
func TestRefreshEmptyClaimRunsInTheWorkbooksWorkspace(t *testing.T) {
	for name, pl := range map[string]map[string]any{
		"payload ws matches": {"workbook_id": "w1", "workspace_id": wsA},
		"payload ws omitted": {"workbook_id": "w1"},
	} {
		t.Run(name, func(t *testing.T) {
			pool, ctx := setupPool(t)
			seedStaleWorkbook(t, ctx, pool, wsA, "w1", refreshCols, `{"enabled":true,"interval":"daily"}`)
			if err := refresh.Handle(ctx, deps(pool), claim(1001, "", pl)); err != nil {
				t.Fatal(err)
			}
			if n := len(ofType(jobs(t, ctx, pool, wsA), "run_workbook")); n != 1 {
				t.Errorf("run_workbook jobs in the workbook's workspace = %d, want 1", n)
			}
			if n := len(ofType(jobs(t, ctx, pool, wsA), "refresh_workbook")); n != 1 {
				t.Errorf("chained refresh jobs = %d, want 1", n)
			}
			if n, _ := activityCount(t, ctx, pool, wsA); n != 1 {
				t.Errorf("activity rows = %d, want 1", n)
			}
		})
	}
}

// An empty claim whose payload names a workspace other than the workbook's is refused with no side effects.
func TestRefreshEmptyClaimPayloadMismatchRefused(t *testing.T) {
	pool, ctx := setupPool(t)
	seedStaleWorkbook(t, ctx, pool, wsA, "w1", refreshCols, `{"enabled":true,"interval":"daily"}`)
	err := refresh.Handle(ctx, deps(pool), claim(1002, "", map[string]any{"workbook_id": "w1", "workspace_id": wsB}))
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
}

// An empty claim for a workbook that does not exist is a permanent error (nothing to derive a tenant from).
func TestRefreshEmptyClaimMissingWorkbookRefused(t *testing.T) {
	pool, ctx := setupPool(t)
	err := refresh.Handle(ctx, deps(pool), claim(1003, "", map[string]any{"workbook_id": "nope", "workspace_id": wsA}))
	var perm *queue.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("err = %v, want a PermanentError", err)
	}
}

// OnFailure follows the same fallback: empty notice workspace re-seeds in the workbook's workspace.
func TestRefreshFailureEmptyNoticeWorkspaceUsesWorkbookWorkspace(t *testing.T) {
	pool, ctx := setupPool(t)
	seedStaleWorkbook(t, ctx, pool, wsA, "w1", refreshCols, `{"enabled":true,"interval":"daily"}`)
	payload, _ := json.Marshal(map[string]any{"workbook_id": "w1", "workspace_id": wsA})
	n := queue.FailureNotice{JobID: 4300, JobType: "refresh_workbook", WorkspaceID: "", Payload: payload}
	if err := refresh.OnFailure(ctx, deps(pool), n); err != nil {
		t.Fatal(err)
	}
	if got := len(ofType(jobs(t, ctx, pool, wsA), "refresh_workbook")); got != 1 {
		t.Fatalf("want one re-seeded job in the workbook's workspace, got %d", got)
	}
}

func TestRefreshFailureEmptyNoticeMismatchOrMissingDoesNotReseed(t *testing.T) {
	pool, ctx := setupPool(t)
	seedStaleWorkbook(t, ctx, pool, wsA, "w1", refreshCols, `{"enabled":true,"interval":"daily"}`)
	for i, pl := range []map[string]any{
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
