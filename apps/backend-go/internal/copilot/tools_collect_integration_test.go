//go:build integration

package copilot

import (
	"context"
	"encoding/json"
	"testing"
)

// start_collection enqueues through collect.Enqueue with the cohort executor (GO_EXECUTOR_COLLECT + cohort),
// the ledger row and the queue row in one transaction, workspace_id always stamped.
func TestStartCollectionUsesCohortExecutor(t *testing.T) {
	e := toolsSetup(t)
	ctx := context.Background()
	clean := func() {
		for _, q := range []string{`DELETE FROM jobs WHERE workspace_id = $1`, `DELETE FROM collection_jobs WHERE workspace_id = $1`} {
			_, _ = e.owner.Exec(ctx, q, e.ws)
		}
	}
	clean()
	t.Cleanup(clean)
	for _, exec := range []string{"go", "python"} {
		tn := e.turn()
		tn.UID = i32(2)
		var asked string
		tn.Engine = &Engine{CollectionLedgerPG: true, CollectExecutor: func(ws string) string { asked = ws; return exec }}
		res, handled, err := tn.engineTool(ctx, parsedCall{Name: "start_collection", Args: map[string]any{"query": "IT staffing companies in Pune " + exec}})
		if err != nil || !handled {
			t.Fatalf("%s: handled=%v err=%v", exec, handled, err)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(res), &out)
		if out["ok"] != true || asked != e.ws {
			t.Fatalf("%s: %s (asked %q)", exec, res, asked)
		}
		var gotExec, ws, payload string
		if err := e.owner.QueryRow(ctx, `SELECT executor, workspace_id, payload::text FROM jobs WHERE id = $1`, int64(out["queue_job_id"].(float64))).
			Scan(&gotExec, &ws, &payload); err != nil {
			t.Fatal(err)
		}
		if gotExec != exec || ws != e.ws {
			t.Fatalf("job executor %q workspace %q, want %q %q", gotExec, ws, exec, e.ws)
		}
		var n int
		if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM collection_jobs WHERE workspace_id = $1 AND id = $2`, e.ws, out["job_id"]).Scan(&n); err != nil || n != 1 {
			t.Fatalf("ledger row: %d %v", n, err)
		}
	}
	// nil CollectExecutor keeps the Python executor
	tn := e.turn()
	tn.UID = i32(2)
	tn.Engine = &Engine{CollectionLedgerPG: true}
	res, _, err := tn.engineTool(ctx, parsedCall{Name: "start_collection", Args: map[string]any{"query": "IT staffing companies in Goa"}})
	var out map[string]any
	_ = json.Unmarshal([]byte(res), &out)
	var gotExec string
	if err != nil || e.owner.QueryRow(ctx, `SELECT executor FROM jobs WHERE id = $1`, int64(out["queue_job_id"].(float64))).Scan(&gotExec) != nil || gotExec != "python" {
		t.Fatalf("default executor %q (%s, %v)", gotExec, res, err)
	}
}
