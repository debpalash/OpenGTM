//go:build integration

package ambitionimport_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ambitionimport"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

func notice(ws string, payload map[string]any, reason string, willRetry bool) queue.FailureNotice {
	b, _ := json.Marshal(payload)
	return queue.FailureNotice{JobID: 7, JobType: "ambitionbox_import", WorkspaceID: ws, Payload: b, Reason: reason, WillRetry: willRetry}
}

func TestReconcileRetryVsFinal(t *testing.T) {
	vec := loadVectors(t)
	if len(vec.Reconcile) != 8 {
		t.Fatalf("expected 8 recorded reconcile cases, got %d", len(vec.Reconcile))
	}
	for _, rc := range vec.Reconcile {
		rc := rc
		t.Run(rc.Name, func(t *testing.T) {
			e := newEnv(t)
			res, err := ambitionimport.Start(e.ctx, e.d, ambitionimport.StartParams{WorkspaceID: wsA, Name: "Vec Recon", SortBy: "popular", Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			e.in(wsA, func(tx pgx.Tx) error {
				_, err := tx.Exec(e.ctx, `UPDATE connector_runs SET status=$2 WHERE id=$1`, res.RunID, rc.InitialStatus)
				return err
			})
			payload := map[string]any{"workspace_id": wsA, "workbook_id": res.WorkbookID, "run_id": res.RunID}
			for k, v := range rc.PayloadOverride {
				payload[k] = v
			}
			reason := strings.Repeat("x", rc.ErrorLength)
			if rc.ErrorLength == 4 { // "boom"
				reason = "boom"
			} else if rc.ErrorLength == 1 {
				reason = "e"
			} else if rc.ErrorLength == 12 {
				reason = "late failure"
			}
			if err := ambitionimport.Reconcile(e.ctx, e.d, notice(wsA, payload, reason, rc.WillRetry)); err != nil {
				t.Fatal(err)
			}
			assertEq(t, "run", e.runView(wsA, res.RunID), rc.Run)
			wbv, rows := e.wbView(wsA, res.WorkbookID, res.RunID)
			assertEq(t, "workbook", wbv, rc.Workbook)
			assertEq(t, "rows", rows, rc.Rows)
		})
	}
}

func TestReconcileNoopOnComplete(t *testing.T) {
	e := newEnv(t)
	res, _ := startRun(e, 10)
	e.in(wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `UPDATE connector_runs SET status='complete', completed_at=now() WHERE id=$1`, res.RunID)
		return err
	})
	before := e.runView(wsA, res.RunID)
	wb0, _ := e.wbView(wsA, res.WorkbookID, res.RunID)
	p := map[string]any{"workspace_id": wsA, "workbook_id": res.WorkbookID, "run_id": res.RunID}
	for _, wr := range []bool{true, false} {
		if err := ambitionimport.Reconcile(e.ctx, e.d, notice(wsA, p, "late", wr)); err != nil {
			t.Fatal(err)
		}
	}
	assertEq(t, "run", e.runView(wsA, res.RunID), before)
	wb1, _ := e.wbView(wsA, res.WorkbookID, res.RunID)
	assertEq(t, "workbook", wb1, wb0)
	// Malformed payloads are logged and ignored.
	if err := ambitionimport.Reconcile(e.ctx, e.d, queue.FailureNotice{JobID: 1, WorkspaceID: wsA, Payload: []byte(`not json`), Reason: "x"}); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileTenantScoped(t *testing.T) {
	e := newEnv(t)
	res, _ := startRun(e, 10)
	before := e.runView(wsA, res.RunID)
	// A payload that claims workspace B but names workspace A's run must not touch it.
	p := map[string]any{"workspace_id": wsB, "workbook_id": res.WorkbookID, "run_id": res.RunID}
	if err := ambitionimport.Reconcile(e.ctx, e.d, notice(wsB, p, "boom", false)); err != nil {
		t.Fatal(err)
	}
	// And a job row of workspace B carrying A's ids in its payload.
	pa := map[string]any{"workspace_id": wsA, "workbook_id": res.WorkbookID, "run_id": res.RunID}
	if err := ambitionimport.Reconcile(e.ctx, e.d, notice(wsB, pa, "boom", false)); err != nil {
		t.Fatal(err)
	}
	assertEq(t, "run", e.runView(wsA, res.RunID), before)
	wbv, _ := e.wbView(wsA, res.WorkbookID, res.RunID)
	if wbv["status"] != "running" {
		t.Fatalf("workbook %+v", wbv)
	}
}

func TestReconcileRegisteredAsFailureHook(t *testing.T) {
	e := newEnv(t)
	ambitionimport.RegisterFailureHook(e.d)
	defer queue.RegisterFailureHook("ambitionbox_import", nil)
	e.fetch.pages[1] = pageOf(1, ids(1, 20), ip(5), ip(100), nil)
	e.fetch.fail[1] = "boom"
	res, c := startRun(e, 5)
	err := ambitionimport.Handle(e.ctx, e.d, c)
	if err == nil {
		t.Fatal("want error")
	}
	// processJob path: permanent-free failed outcome, retries exhausted -> final.
	if _, err := e.owner.Exec(e.ctx, `UPDATE jobs SET retry_count = max_retries WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.FinalizeAndNotify(e.ctx, e.pool, nil, c, queue.Outcome{Status: "failed", Error: err.Error()}); err != nil {
		t.Fatal(err)
	}
	run := e.runView(wsA, res.RunID)
	if run["status"] != "failed" || run["error"] != "boom" || run["completed_at"] != true {
		t.Fatalf("run %+v", run)
	}
}
