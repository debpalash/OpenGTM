//go:build integration

package playbooks

import (
	"context"
	"testing"
	"time"
)

// F18: a final-failure reconcile on a run that is already failed is a no-op: the
// first failure's error, finished_at, counts and result rows stay as they were.
func TestReconcileFinalOnFailedRunIsNoop(t *testing.T) {
	e := setup(t)
	_, run := e.engineRun(t, oneStep, 100)
	e.seedLedger(t, run.ID)
	e.sql(t, e.ws, `UPDATE playbook_runs SET status='running', finished_at=NULL WHERE workspace_id=$1 AND id=$2`, e.ws, run.ID)
	if err := ReconcileFailure(context.Background(), e.engineDeps(nil), 7, payloadFor(e.ws, run.ID), "first", false); err != nil {
		t.Fatal(err)
	}
	before := e.runState(t, e.ws, run.ID)
	if before.status != "failed" || before.err == nil || *before.err != "Final failure: first" {
		t.Fatalf("setup: %+v", before)
	}
	// a result row reset by hand must not be re-failed by the second reconcile.
	e.sql(t, e.ws, `UPDATE playbook_results SET status='pending', error='keep' WHERE workspace_id=$1 AND run_id=$2 AND lead_id=4`, e.ws, run.ID)
	time.Sleep(20 * time.Millisecond)
	if err := ReconcileFailure(context.Background(), e.engineDeps(nil), 8, payloadFor(e.ws, run.ID), "second", false); err != nil {
		t.Fatal(err)
	}
	after := e.runState(t, e.ws, run.ID)
	if after.status != "failed" || after.err == nil || *after.err != "Final failure: first" ||
		after.finished == nil || !after.finished.Equal(*before.finished) ||
		[3]int{after.attempted, after.succeeded, after.failed} != [3]int{before.attempted, before.succeeded, before.failed} {
		t.Fatalf("failed run re-processed: before %+v after %+v", before, after)
	}
	for _, r := range e.results(t, e.ws, run.ID) {
		if r.lead == 4 && (r.status != "pending" || r.err == nil || *r.err != "keep") {
			t.Fatalf("result re-failed: %+v", r)
		}
	}
}
