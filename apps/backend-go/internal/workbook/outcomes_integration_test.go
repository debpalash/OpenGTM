//go:build integration

package workbook_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// Providers ran and found nothing: a normal result. The run is complete, not failed, and the cells say no_data.
func TestRunWhereCellsFoundNoDataIsComplete(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-o1", "["+colJSON("c1", "free1")+"]", 2, 0)
	s := newSim(t, func(_ string, _ int, w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"v":""}`) })
	c := e.claim(wsA, payloadFor(wsA, "wb-o1", map[string]float64{"free1": 0}, nil))
	res, err := workbook.Run(e.ctx, e.deps(s, nil), c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed != 0 || res.NoData != 2 || res.NeedsSetup != 0 || res.Errors != 0 {
		t.Fatalf("counts %+v, want 0 filled, 2 no data, 0 errors", res)
	}
	if st := e.str(wsA, `SELECT status FROM workbooks WHERE id='wb-o1'`); st != "complete" {
		t.Fatalf("workbook status %q, want complete", st)
	}
	// The stored cell keeps its status and text (rows written by earlier versions read the same way).
	if n := e.one(wsA, `SELECT count(*) FROM workbook_enrichments WHERE status='error' AND error='no_data'`); n != 2 {
		t.Fatalf("stored cells %d", n)
	}
}

// A no_data cell is not retried: nothing failed, so a retry pass has nothing to do and the provider is asked once.
func TestRetryPassDoesNotRerunNoDataCells(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-o2", "["+colJSON("c1", "free1")+"]", 2, 0)
	s := newSim(t, func(_ string, _ int, w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"v":""}`) })
	c := e.claim(wsA, payloadFor(wsA, "wb-o2", map[string]float64{"free1": 0}, map[string]any{"retry_passes": 1}))
	if _, err := workbook.Run(e.ctx, e.deps(s, nil), c); err != nil {
		t.Fatal(err)
	}
	if got := s.count("free1"); got != 2 {
		t.Fatalf("provider called %d times for 2 rows; a no_data cell was retried", got)
	}
}

func mixedSim(t *testing.T) *sim {
	return newSim(t, func(name string, _ int, w http.ResponseWriter, _ *http.Request) {
		if name == "bad1" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"v":"1999"}`)
	})
}

// Some cells filled and some failed with a provider error: complete with errors, not failed.
func TestRunWithSomeErrorCellsIsCompletedWithErrors(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-o3", "["+colJSON("c1", "good1")+","+colJSON("c2", "bad1")+"]", 2, 0)
	c := e.claim(wsA, payloadFor(wsA, "wb-o3", map[string]float64{"good1": 0, "bad1": 0}, map[string]any{"retry_passes": 0}))
	res, err := workbook.Run(e.ctx, e.deps(mixedSim(t), nil), c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed != 2 || res.Errors != 2 {
		t.Fatalf("counts %+v, want 2 filled and 2 errors", res)
	}
	if st := e.str(wsA, `SELECT status FROM workbooks WHERE id='wb-o3'`); st != "completed_with_errors" {
		t.Fatalf("workbook status %q, want completed_with_errors", st)
	}
}

// Every attempted cell failed with a provider error: the run failed.
func TestRunWhereEveryCellErroredFails(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-o4", "["+colJSON("c1", "bad1")+"]", 2, 0)
	c := e.claim(wsA, payloadFor(wsA, "wb-o4", map[string]float64{"bad1": 0}, map[string]any{"retry_passes": 0}))
	res, err := workbook.Run(e.ctx, e.deps(mixedSim(t), nil), c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed != 0 || res.Errors != 2 {
		t.Fatalf("counts %+v", res)
	}
	if st := e.str(wsA, `SELECT status FROM workbooks WHERE id='wb-o4'`); st != "failed" {
		t.Fatalf("workbook status %q, want failed", st)
	}
}

// A retry pass still picks up the cells that ended in an error.
func TestRetryPassStillRerunsErrorCells(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-o5", "["+colJSON("c1", "bad1")+"]", 1, 0)
	s := mixedSim(t)
	c := e.claim(wsA, payloadFor(wsA, "wb-o5", map[string]float64{"bad1": 0}, map[string]any{"retry_passes": 1}))
	if _, err := workbook.Run(e.ctx, e.deps(s, nil), c); err != nil {
		t.Fatal(err)
	}
	if got := s.count("bad1"); got != 2 {
		t.Fatalf("provider called %d times, want 2 (first pass + one retry pass)", got)
	}
}
