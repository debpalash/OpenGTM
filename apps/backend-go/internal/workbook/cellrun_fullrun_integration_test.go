//go:build integration

package workbook_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// With GO_EXECUTOR_FULL_RUN the single-cell route never proxies to the Python API:
// output columns and email verification run in process, and a column whose Python worker
// is unavailable becomes an error cell instead of ErrNeedsPython.
func TestCellRunFullRunNeverNeedsPython(t *testing.T) {
	e := setup(t, wsA)
	cols := `[{"id":"o","type":"output","name":"O","destination":"sequencer","destination_config":{"sequence_id":"s1"}},
	  {"id":"em","type":"waterfall","name":"Em","target_field":"email","waterfall":["hunter_io"]},
	  {"id":"ai","type":"ai_formula","name":"AI","prompt":"hi {company}"}]`
	ids := e.seed(wsA, "wb-full", cols, 1, 0)
	d := e.deps(newSim(t, nil), nil)
	d.Runner = &fakeRunner{fn: func(context.Context, string, providers.Input) (providers.Output, error) {
		return providers.Output{Confidence: 0.9, Extra: obj("email", "a@acme0.example.com")}, nil
	}}
	rec := &recorder{}
	fp := fakePorts{rec: rec, dest: map[string]json.RawMessage{"sequencer": json.RawMessage(`{}`)}}
	d.Output = &workbook.OutputPorts{Outreach: outreachFake{fp}}
	d.Verify = workbook.VerifyFunc(func(context.Context, string, string) (emailverify.Result, error) {
		return emailverify.Result{Status: "valid", Source: "smtp", Confidence: 0.95}, nil
	})
	req := func(col string) workbook.CellRequest {
		return workbook.CellRequest{WorkspaceID: wsA, WorkbookID: "wb-full", RowID: int64(ids[0]), ColumnID: col}
	}
	// flag off: today's proxy answers
	for _, c := range []string{"o", "em", "ai"} {
		if _, err := workbook.EnrichCell(e.ctx, d, req(c)); !errors.Is(err, workbook.ErrNeedsPython) {
			t.Fatalf("%s with the flag off must proxy: %v", c, err)
		}
	}
	d.FullRun = true
	r, err := workbook.EnrichCell(e.ctx, d, req("em"))
	if err != nil || r.Status() != "complete" || r.Value != "a@acme0.example.com" {
		t.Fatalf("email cell: %+v %v %v", r, err, deref(r.Error))
	}
	if got := e.str(wsA, `SELECT enrichments::jsonb #>> '{em,verify_status}' FROM workbook_rows WHERE id = $1`, ids[0]); got != "valid" {
		t.Fatalf("verify_status %q", got)
	}
	// the sequencer needs an email on the row: the cell above wrote "em", not "email"
	r, err = workbook.EnrichCell(e.ctx, d, req("o"))
	if err != nil || r.Status() != "error" || r.Error == nil || *r.Error != "no_email" {
		t.Fatalf("output cell: %+v %v", r, err)
	}
	r, err = workbook.EnrichCell(e.ctx, d, req("ai"))
	if err != nil || r.Status() != "error" || r.Error == nil || *r.Error != "python_worker_unavailable" {
		t.Fatalf("ai cell without a python worker: %+v %v", r, err)
	}
	if got := e.str(wsA, `SELECT enrichments::jsonb #>> '{ai,error}' FROM workbook_rows WHERE id = $1`, ids[0]); got != "python_worker_unavailable" {
		t.Fatalf("ai error cell %q", got)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
