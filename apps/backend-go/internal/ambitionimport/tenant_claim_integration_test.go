//go:build integration

package ambitionimport_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ambitionimport"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

// Final security Low 1: the tenant is the claim's only. A payload naming another workspace's live run, or a
// claim without a workspace, is refused permanently before anything is read or fetched.
func TestHandleTakesTenantFromClaimOnly(t *testing.T) {
	e := newEnv(t)
	e.seedWorkbook(wsB, "wb-victim", "B", "ambitionbox", nil)
	e.in(wsB, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO connector_runs (id, workspace_id, workbook_id, connector, status, query, cursor, requested_count,
			fetched_count, added_count, updated_count, skipped_count, pages_fetched, target_met, exhausted, started_at)
			VALUES ('run-victim',$1,'wb-victim','ambitionbox','queued','{"max_pages": 1}'::json,'{}'::json,10,0,0,0,0,0,false,false,now())`, wsB)
		return err
	})
	e.fetch.pages[1] = pageOf(1, ids(1, 5), ip(1), ip(5), nil)
	forged := `{"workspace_id":"` + wsB + `","workbook_id":"wb-victim","run_id":"run-victim"}`
	permanent := func(name string, c *queue.Claim) {
		t.Helper()
		err := ambitionimport.Handle(e.ctx, e.d, c)
		var pe *queue.PermanentError
		if err == nil || !errors.As(err, &pe) {
			t.Fatalf("%s: want a permanent refusal, got %v", name, err)
		}
	}
	permanent("payload names another workspace", e.claimPayload(wsA, forged))
	c := e.claimPayload(wsA, forged)
	c.WorkspaceID = ""
	c.Payload = json.RawMessage(forged)
	permanent("claim without a workspace", c)
	if len(e.fetch.callPages()) != 0 {
		t.Fatal("fetched for a forged payload")
	}
	if run := e.runView(wsB, "run-victim"); run["status"] != "queued" {
		t.Fatalf("victim run touched: %+v", run)
	}
}

// Final security Low 1: the failure hook touches nothing for a job without a workspace.
func TestReconcileIgnoresEmptyJobWorkspace(t *testing.T) {
	e := newEnv(t)
	e.seedWorkbook(wsB, "wb-victim2", "B", "ambitionbox", nil)
	e.in(wsB, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO connector_runs (id, workspace_id, workbook_id, connector, status, query, cursor, requested_count,
			fetched_count, added_count, updated_count, skipped_count, pages_fetched, target_met, exhausted, started_at)
			VALUES ('run-victim2',$1,'wb-victim2','ambitionbox','running','{"max_pages": 1}'::json,'{}'::json,10,0,0,0,0,0,false,false,now())`, wsB)
		return err
	})
	n := queue.FailureNotice{JobID: 1, Payload: json.RawMessage(`{"workspace_id":"` + wsB + `","workbook_id":"wb-victim2","run_id":"run-victim2"}`),
		Reason: "forged", WillRetry: false}
	if err := ambitionimport.Reconcile(e.ctx, e.d, n); err != nil {
		t.Fatal(err)
	}
	if run := e.runView(wsB, "run-victim2"); run["status"] != "running" {
		t.Fatalf("victim run touched: %+v", run)
	}
}
