//go:build integration

package workbook_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/spend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// hunter_io is a catalog vendor with a known paid cost (0.04), so the in-request
// cell run reserves spend for it exactly like the queued run does.
const paidCol = `[{"id":"c1","type":"waterfall","name":"C1","target_field":"founded_year","waterfall":["hunter_io"]}]`

func paidRunner(delay time.Duration) *fakeRunner {
	return &fakeRunner{fn: func(ctx context.Context, _ string, _ providers.Input) (providers.Output, error) {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return providers.Output{}, ctx.Err()
		}
		return providers.Output{Confidence: 0.7, Extra: obj("founded_year", "1999")}, nil
	}}
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// A cell run and a queued run of the same cell share the spend attempt key of
// the live job: the provider is paid and called once whichever goes first.
func TestCellRunRaceWithQueuedRunSpendsOnce(t *testing.T) {
	for _, mode := range []string{"cell_first", "concurrent"} {
		t.Run(mode, func(t *testing.T) {
			e := setup(t, wsA)
			ids := e.seed(wsA, "wb-race", paidCol, 1, 10)
			fr := paidRunner(0)
			if mode == "concurrent" {
				fr = paidRunner(150 * time.Millisecond)
			}
			d := e.deps(newSim(t, nil), nil)
			d.Runner = fr
			c := e.claim(wsA, payloadNoManifests(wsA, "wb-race", map[string]float64{"hunter_io": 0.04}, nil))
			req := workbook.CellRequest{WorkspaceID: wsA, WorkbookID: "wb-race", RowID: int64(ids[0]), ColumnID: "c1"}

			var cellRes workbook.CellResult
			var cellErr error
			if mode == "cell_first" {
				cellRes, cellErr = workbook.EnrichCell(e.ctx, d, req)
				if cellErr != nil || !cellRes.Success || cellRes.Value != "1999" {
					t.Fatalf("cell run: %+v %v", cellRes, cellErr)
				}
				if _, err := workbook.Run(e.ctx, d, c); err != nil {
					t.Fatal(err)
				}
			} else {
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { defer wg.Done(); cellRes, cellErr = workbook.EnrichCell(e.ctx, d, req) }()
				go func() { defer wg.Done(); _, _ = workbook.Run(e.ctx, d, c) }()
				wg.Wait()
			}
			if n := fr.callCount(); n != 1 {
				t.Fatalf("provider called %d times, want 1 (cell: %+v %v)", n, cellRes, cellErr)
			}
			if n := e.one(wsA, `SELECT count(*) FROM workbook_spend_attempts`); n != 1 {
				t.Fatalf("%d spend attempts, want 1", n)
			}
			if got := e.spentUSD(wsA, "wb-race"); got != 0.04 {
				t.Fatalf("spent %v, want 0.04 once", got)
			}
			want := spend.AttemptKey(wsA, "wb-race", c.ID, fmt.Sprintf("row:%d", ids[0]), "c1", "hunter_io")
			if got := e.str(wsA, `SELECT attempt_key FROM workbook_spend_attempts`); got != want {
				t.Fatalf("attempt key %q is not the queued run's key %q", got, want)
			}
		})
	}
}

// Without any live job the cell run still reserves (never an unguarded paid
// call), under its own cell identity; a settled call is paid once.
func TestCellRunWithoutJobReservesAndSettles(t *testing.T) {
	e := setup(t, wsA)
	ids := e.seed(wsA, "wb-nojob", paidCol, 1, 10)
	fr := paidRunner(0)
	d := e.deps(newSim(t, nil), nil)
	d.Runner = fr
	res, err := workbook.EnrichCell(e.ctx, d, workbook.CellRequest{WorkspaceID: wsA, WorkbookID: "wb-nojob", RowID: int64(ids[0]), ColumnID: "c1"})
	if err != nil || !res.Success || res.Value != "1999" || res.Provider == nil || *res.Provider != "hunter_io" {
		t.Fatalf("%+v %v", res, err)
	}
	if st := e.str(wsA, `SELECT status FROM workbook_spend_attempts`); st != "settled" {
		t.Fatalf("attempt %q", st)
	}
	if got := e.spentUSD(wsA, "wb-nojob"); got != 0.04 {
		t.Fatalf("spent %v", got)
	}
	if got := overlay(e, "wb-nojob", "c1", "status"); got != "complete" {
		t.Fatalf("overlay status %q", got)
	}
}

// A client disconnect (request context cancel) mid paid call leaves the
// attempt uncertain and writes no cell, like a lease loss in the queued run.
func TestCellRunCancelOnClientDisconnect(t *testing.T) {
	e := setup(t, wsA)
	ids := e.seed(wsA, "wb-dc", paidCol, 1, 10)
	started := make(chan struct{})
	fr := &fakeRunner{fn: func(ctx context.Context, _ string, _ providers.Input) (providers.Output, error) {
		close(started)
		<-ctx.Done()
		return providers.Output{}, ctx.Err()
	}}
	d := e.deps(newSim(t, nil), nil)
	d.Runner = fr
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan error, 1)
	go func() {
		_, err := workbook.EnrichCell(ctx, d, workbook.CellRequest{WorkspaceID: wsA, WorkbookID: "wb-dc", RowID: int64(ids[0]), ColumnID: "c1"})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil || ctx.Err() == nil {
			t.Fatalf("want a cancellation error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EnrichCell did not return after the context was cancelled")
	}
	if st := e.str(wsA, `SELECT status FROM workbook_spend_attempts`); st != "uncertain" {
		t.Fatalf("attempt %q, want uncertain", st)
	}
	if got := overlay(e, "wb-dc", "c1", "status"); got != "" {
		t.Fatalf("a cancelled cell was written: %q", got)
	}
}

func TestCellRunSentinels(t *testing.T) {
	const wsOther = "ws-t12-b"
	e := setup(t, wsA, wsOther)
	ids := e.seed(wsA, "wb-s", `[{"id":"c1","type":"formula","name":"C1","formula":"len({company})"},
	  {"id":"o1","type":"output","name":"O","destination":"webhook"},
	  {"id":"w1","type":"waterfall","name":"W","target_field":"email","waterfall":["hunter_io"]},
	  {"id":"lf","type":"lead_field","name":"LF","lead_field":"company"}]`, 1, 10)
	d := e.deps(newSim(t, nil), nil)
	d.Runner = paidRunner(0)
	cases := []struct {
		name string
		req  workbook.CellRequest
		want error
	}{
		{"workbook", workbook.CellRequest{WorkspaceID: wsA, WorkbookID: "nope", RowID: int64(ids[0]), ColumnID: "c1"}, workbook.ErrWorkbookNotFound},
		{"column", workbook.CellRequest{WorkspaceID: wsA, WorkbookID: "wb-s", RowID: int64(ids[0]), ColumnID: "zzz"}, workbook.ErrColumnNotFound},
		{"lead_field column", workbook.CellRequest{WorkspaceID: wsA, WorkbookID: "wb-s", RowID: int64(ids[0]), ColumnID: "lf"}, workbook.ErrColumnNotFound},
		{"row", workbook.CellRequest{WorkspaceID: wsA, WorkbookID: "wb-s", RowID: 99999, ColumnID: "c1"}, workbook.ErrRowNotFound},
		{"output runs on python", workbook.CellRequest{WorkspaceID: wsA, WorkbookID: "wb-s", RowID: int64(ids[0]), ColumnID: "o1"}, workbook.ErrNeedsPython},
		{"email verify runs on python", workbook.CellRequest{WorkspaceID: wsA, WorkbookID: "wb-s", RowID: int64(ids[0]), ColumnID: "w1"}, workbook.ErrNeedsPython},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := workbook.EnrichCell(e.ctx, d, c.req); !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
	if _, err := workbook.EnrichCell(e.ctx, d, workbook.CellRequest{WorkspaceID: wsOther, WorkbookID: "wb-s", RowID: int64(ids[0]), ColumnID: "c1"}); !errors.Is(err, workbook.ErrWorkbookNotFound) {
		t.Fatalf("a foreign workspace must not see the workbook: %v", err)
	}
}
