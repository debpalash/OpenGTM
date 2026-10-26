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
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
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

// hookRunner runs before() between the cell load and the spend reservation (the runner's
// credential precheck is the last step before Reserve).
type hookRunner struct {
	*fakeRunner
	before func()
}

func (h hookRunner) Configured(ctx context.Context, ws, p string) (bool, error) {
	if h.before != nil {
		h.before()
	}
	return h.fakeRunner.Configured(ctx, ws, p)
}

// A queued run that appears after the cell was loaded (and before it reserves) still owns the
// attempt key: the cell re-resolves the live job at reserve time.
func TestCellRunJobAppearingBeforeReserveSharesKey(t *testing.T) {
	e := setup(t, wsA)
	ids := e.seed(wsA, "wb-late", paidCol, 1, 10)
	fr := paidRunner(0)
	d := e.deps(newSim(t, nil), nil)
	var c *queue.Claim
	d.Runner = hookRunner{fr, func() {
		if c == nil {
			c = e.claim(wsA, payloadNoManifests(wsA, "wb-late", map[string]float64{"hunter_io": 0.04}, nil))
		}
	}}
	res, err := workbook.EnrichCell(e.ctx, d, workbook.CellRequest{WorkspaceID: wsA, WorkbookID: "wb-late", RowID: int64(ids[0]), ColumnID: "c1"})
	if err != nil || !res.Success {
		t.Fatalf("%+v %v", res, err)
	}
	want := spend.AttemptKey(wsA, "wb-late", c.ID, fmt.Sprintf("row:%d", ids[0]), "c1", "hunter_io")
	if got := e.str(wsA, `SELECT attempt_key FROM workbook_spend_attempts`); got != want {
		t.Fatalf("attempt key %q is not the late job's key %q", got, want)
	}
	if _, err := workbook.Run(e.ctx, d, c); err != nil {
		t.Fatal(err)
	}
	if n := fr.callCount(); n != 1 {
		t.Fatalf("provider called %d times", n)
	}
	if got := e.spentUSD(wsA, "wb-late"); got != 0.04 {
		t.Fatalf("spent %v", got)
	}
}

// After dispatch the run is detached from the client: a disconnect neither strands the paid
// attempt nor loses the provider result.
func TestCellRunDetachesAfterDispatch(t *testing.T) {
	e := setup(t, wsA)
	ids := e.seed(wsA, "wb-dc", paidCol, 1, 10)
	started, release := make(chan struct{}), make(chan struct{})
	fr := &fakeRunner{fn: func(context.Context, string, providers.Input) (providers.Output, error) {
		close(started)
		<-release
		return providers.Output{Confidence: 0.7, Extra: obj("founded_year", "1999")}, nil
	}}
	d := e.deps(newSim(t, nil), nil)
	d.Runner = fr
	ctx, cancel := context.WithCancel(e.ctx)
	type out struct {
		r   workbook.CellResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := workbook.EnrichCell(ctx, d, workbook.CellRequest{WorkspaceID: wsA, WorkbookID: "wb-dc", RowID: int64(ids[0]), ColumnID: "c1"})
		done <- out{r, err}
	}()
	<-started
	cancel()
	close(release)
	select {
	case o := <-done:
		if o.err != nil || !o.r.Success {
			t.Fatalf("detached run: %+v %v", o.r, o.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EnrichCell did not return")
	}
	if st := e.str(wsA, `SELECT status FROM workbook_spend_attempts`); st != "settled" {
		t.Fatalf("attempt %q, want settled", st)
	}
	if got := overlay(e, "wb-dc", "c1", "status"); got != "complete" {
		t.Fatalf("cell not persisted: %q", got)
	}
}

// A client that is gone before dispatch cancels the run: no attempt, no provider call, no write.
func TestCellRunCancelBeforeDispatchWritesNothing(t *testing.T) {
	e := setup(t, wsA)
	ids := e.seed(wsA, "wb-pre", paidCol, 1, 10)
	fr := paidRunner(0)
	d := e.deps(newSim(t, nil), nil)
	ctx, cancel := context.WithCancel(e.ctx)
	d.Runner = hookRunner{fr, cancel}
	_, err := workbook.EnrichCell(ctx, d, workbook.CellRequest{WorkspaceID: wsA, WorkbookID: "wb-pre", RowID: int64(ids[0]), ColumnID: "c1"})
	if err == nil || ctx.Err() == nil {
		t.Fatalf("want a cancellation error, got %v", err)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_spend_attempts`); n != 0 || fr.callCount() != 0 {
		t.Fatalf("%d attempts, %d calls", n, fr.callCount())
	}
	if got := overlay(e, "wb-pre", "c1", "status"); got != "" {
		t.Fatalf("written: %q", got)
	}
}

// While a live queued run holds the attempt (dispatched), the cell reports
// attempt_not_dispatchable without persisting an error cell the queued run would then overwrite.
func TestCellRunNotDispatchableWithLiveJobIsNotPersisted(t *testing.T) {
	e := setup(t, wsA)
	ids := e.seed(wsA, "wb-nd", paidCol, 1, 10)
	started, release := make(chan struct{}), make(chan struct{})
	fr := &fakeRunner{fn: func(context.Context, string, providers.Input) (providers.Output, error) {
		close(started)
		<-release
		return providers.Output{Confidence: 0.7, Extra: obj("founded_year", "1999")}, nil
	}}
	d := e.deps(newSim(t, nil), nil)
	d.Runner = fr
	c := e.claim(wsA, payloadNoManifests(wsA, "wb-nd", map[string]float64{"hunter_io": 0.04}, nil))
	runDone := make(chan struct{})
	go func() { defer close(runDone); _, _ = workbook.Run(e.ctx, d, c) }()
	<-started
	res, err := workbook.EnrichCell(e.ctx, d, workbook.CellRequest{WorkspaceID: wsA, WorkbookID: "wb-nd", RowID: int64(ids[0]), ColumnID: "c1"})
	if err != nil || res.Success || res.Error == nil || *res.Error != "attempt_not_dispatchable" {
		t.Fatalf("%+v %v", res, err)
	}
	if got := overlay(e, "wb-nd", "c1", "status"); got == "error" {
		t.Fatal("the cell run persisted attempt_not_dispatchable")
	}
	close(release)
	<-runDone
	if got := overlay(e, "wb-nd", "c1", "status"); got != "complete" {
		t.Fatalf("queued run result lost: %q", got)
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
