//go:build integration

package workbook_test

import (
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

const wsB = "ws-t10-b"

// RunRows is the loop of the queued run without the lease: only the asked
// rows and columns run, spend settles once, the workbook ends complete.
func TestRunRowsRunsOnlyAskedRowAndColumn(t *testing.T) {
	e := setup(t, wsA, wsB)
	ids := e.seed(wsA, "wb-rr", paidCol, 3, 10)
	fr := paidRunner(0)
	d := e.deps(newSim(t, nil), nil)
	d.Runner = fr
	sum, err := workbook.RunRows(e.ctx, d, wsA, "wb-rr", []string{"c1"}, []int32{ids[1]}, 10*time.Second)
	if err != nil || sum.Completed != 1 || sum.Errors != 0 || sum.Error != "" {
		t.Fatalf("%+v %v", sum, err)
	}
	if n := fr.callCount(); n != 1 {
		t.Fatalf("provider called %d times", n)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_rows WHERE workbook_id='wb-rr' AND enrichments::jsonb->'c1'->>'status'='complete'`); n != 1 {
		t.Fatalf("%d rows enriched, want 1", n)
	}
	if got := e.spentUSD(wsA, "wb-rr"); got != 0.04 {
		t.Fatalf("spent %v", got)
	}
	if st := e.str(wsA, `SELECT status FROM workbooks WHERE id='wb-rr'`); st != "complete" {
		t.Fatalf("workbook status %q", st)
	}
	// A column that is not asked for never runs.
	sum, err = workbook.RunRows(e.ctx, d, wsA, "wb-rr", []string{"nope"}, []int32{ids[2]}, 10*time.Second)
	if err != nil || sum.Completed != 0 || fr.callCount() != 1 {
		t.Fatalf("%+v %v calls=%d", sum, err, fr.callCount())
	}
}

// Another workspace's workbook is invisible: not found, nothing runs or is paid.
func TestRunRowsTenantIsolation(t *testing.T) {
	e := setup(t, wsA, wsB)
	ids := e.seed(wsA, "wb-iso", paidCol, 1, 10)
	fr := paidRunner(0)
	d := e.deps(newSim(t, nil), nil)
	d.Runner = fr
	sum, err := workbook.RunRows(e.ctx, d, wsB, "wb-iso", []string{"c1"}, []int32{ids[0]}, 10*time.Second)
	if err != nil || sum.Error != "workbook_not_found" || fr.callCount() != 0 {
		t.Fatalf("%+v %v calls=%d", sum, err, fr.callCount())
	}
	if got := e.spentUSD(wsA, "wb-iso"); got != 0 {
		t.Fatalf("spent %v", got)
	}
}

// RunRows cannot run without a provider runner: an error, not a nil dereference.
func TestRunRowsNeedsRunner(t *testing.T) {
	e := setup(t, wsA)
	d := e.deps(newSim(t, nil), nil)
	if _, err := workbook.RunRows(e.ctx, d, wsA, "wb-x", []string{"c1"}, []int32{1}, time.Second); err == nil {
		t.Fatal("expected error")
	}
}
