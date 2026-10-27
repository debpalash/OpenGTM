//go:build integration

package workbook_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crmpush"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// gateCRM blocks the first push until released so a second request overlaps it.
type gateCRM struct {
	fakePorts
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gateCRM) Push(ctx context.Context, ws string, cfg, lead map[string]any, wb string, leadID int64) crmpush.Result {
	g.once.Do(func() { close(g.entered); <-g.release })
	return g.fakePorts.Push(ctx, ws, cfg, lead, wb, leadID)
}

func crmCellEnv(t *testing.T, wb string) (*env, workbook.Deps, *recorder, int32) {
	e := setup(t, wsA)
	cols := `[{"id":"o","type":"output","name":"O","destination":"crm","destination_config":{}}]`
	ids := e.seed(wsA, wb, cols, 1, 0)
	d := e.deps(newSim(t, nil), nil)
	d.FullRun = true
	rec := &recorder{}
	fp := fakePorts{rec: rec, dest: map[string]json.RawMessage{"crm": json.RawMessage(`{"success":true,"value":"pushed"}`)}}
	d.Output = &workbook.OutputPorts{CRM: fp}
	return e, d, rec, ids[0]
}

// M2: two overlapping single-cell output runs deliver once.
func TestCellRunOutputConcurrentDeliversOnce(t *testing.T) {
	e, d, rec, row := crmCellEnv(t, "wb-m2a")
	g := &gateCRM{fakePorts: fakePorts{rec: rec, dest: map[string]json.RawMessage{"crm": json.RawMessage(`{"success":true,"value":"pushed"}`)}},
		entered: make(chan struct{}), release: make(chan struct{})}
	d.Output = &workbook.OutputPorts{CRM: g}
	req := workbook.CellRequest{WorkspaceID: wsA, WorkbookID: "wb-m2a", RowID: int64(row), ColumnID: "o"}
	first := make(chan workbook.CellResult, 1)
	go func() {
		r, err := workbook.EnrichCell(e.ctx, d, req)
		if err != nil {
			t.Error(err)
		}
		first <- r
	}()
	<-g.entered
	r2, err := workbook.EnrichCell(e.ctx, d, req)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Status() != "error" || deref(r2.Error) != "output_run_in_progress" {
		t.Fatalf("overlapping run: %+v %s", r2, deref(r2.Error))
	}
	close(g.release)
	select {
	case r1 := <-first:
		if r1.Status() != "complete" {
			t.Fatalf("first run: %+v %s", r1, deref(r1.Error))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first run did not finish")
	}
	if len(rec.calls) != 1 {
		t.Fatalf("destination called %d times", len(rec.calls))
	}
	// a later plain run is the run-once guard's skip, still one delivery
	r3, err := workbook.EnrichCell(e.ctx, d, req)
	if err != nil || !r3.Skipped || len(rec.calls) != 1 {
		t.Fatalf("third run: %+v %v calls=%d", r3, err, len(rec.calls))
	}
}

// M2: while a run_workbook job of the workbook is processing, a single-cell output run is refused.
func TestCellRunOutputRefusedWhileJobProcessing(t *testing.T) {
	e, d, rec, row := crmCellEnv(t, "wb-m2b")
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO jobs (type, payload, status, priority, created_at, retry_count, max_retries, workspace_id, executor)
	  VALUES ('run_workbook', $1::json, 'processing', 0, now() AT TIME ZONE 'UTC', 0, 3, $2, 'go')`,
		`{"workspace_id":"`+wsA+`","workbook_id":"wb-m2b"}`, wsA); err != nil {
		t.Fatal(err)
	}
	r, err := workbook.EnrichCell(e.ctx, d, workbook.CellRequest{WorkspaceID: wsA, WorkbookID: "wb-m2b", RowID: int64(row), ColumnID: "o", Force: true})
	if err != nil || r.Status() != "error" || deref(r.Error) != "workbook_run_in_progress" {
		t.Fatalf("%+v %v %s", r, err, deref(r.Error))
	}
	if len(rec.calls) != 0 {
		t.Fatal("destination must not be called")
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_enrichments WHERE workbook_id = 'wb-m2b'`); n != 0 {
		t.Fatalf("refusal wrote %d cells", n)
	}
}

// M2: a claim without a receipt (crashed run) is never resent silently; force re-sends.
func TestCellRunOutputStaleClaimNeedsReview(t *testing.T) {
	e, d, rec, row := crmCellEnv(t, "wb-m2c")
	claim := func(at time.Time) {
		e.inWS(wsA, func(tx pgx.Tx) error {
			_, err := tx.Exec(e.ctx, `INSERT INTO workbook_enrichments (workbook_id, workspace_id, lead_id, column_id, status, cell_metadata)
			  VALUES ('wb-m2c', $1, $3, 'o', 'running', $2::json)
			  ON CONFLICT (workbook_id, lead_id, column_id) DO UPDATE SET status = 'running', cell_metadata = EXCLUDED.cell_metadata`,
				wsA, `{"output_claim":{"claimed_at":"`+at.UTC().Format(time.RFC3339)+`"}}`, row)
			return err
		})
	}
	req := workbook.CellRequest{WorkspaceID: wsA, WorkbookID: "wb-m2c", RowID: int64(row), ColumnID: "o"}
	claim(time.Now().Add(-time.Hour))
	r, err := workbook.EnrichCell(e.ctx, d, req)
	if err != nil || deref(r.Error) != "output_delivery_requires_review" || len(rec.calls) != 0 {
		t.Fatalf("stale claim: %+v %v calls=%d", r, err, len(rec.calls))
	}
	if got := e.str(wsA, `SELECT status FROM workbook_enrichments WHERE workbook_id = 'wb-m2c' AND column_id = 'o'`); got != "error" {
		t.Fatalf("stale claim must become an error cell, got %q", got)
	}
	claim(time.Now().Add(-time.Hour))
	req.Force = true
	r, err = workbook.EnrichCell(e.ctx, d, req)
	if err != nil || r.Status() != "complete" || len(rec.calls) != 1 {
		t.Fatalf("forced: %+v %v calls=%d", r, err, len(rec.calls))
	}
}
