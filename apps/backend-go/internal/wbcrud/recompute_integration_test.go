//go:build integration

package wbcrud_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/billing"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/runplan"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/runstart"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/vendorcatalog"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const recomputeRunID = "recomputerun0001"

var platChain = map[string][]string{"plat_x": {"research", "hunter_io"}}

type recEnv struct {
	rowsEnv
	wb   string
	row  int64
	cost float64
}

func newRecEnv(t *testing.T) recEnv {
	t.Helper()
	e := newRowsEnv(t)
	// research is the platform-billed vendor (RESEARCH_VENDOR_COST on) through the production wiring.
	runplan.Wire(runplan.WireDeps{AppSetting: func(context.Context, string, string) (string, error) { return "1", nil }})
	cleanup := func() {
		for _, sql := range []string{`DELETE FROM jobs WHERE type='run_workbook'`, `DELETE FROM credit_ledger_entries`, `DELETE FROM workspace_credits`} {
			e.exec(wsA, sql)
		}
	}
	cleanup()
	t.Cleanup(func() {
		cleanup()
		runplan.Wire(runplan.WireDeps{})
		vendorcatalog.SetResearchFlag(nil)
	})
	wb := e.workbook(wsA, "rec", `[{"id":"company","name":"Company","type":"lead_field","lead_field":"company"},`+
		`{"id":"plat_x","name":"Platform Chain","type":"waterfall","waterfall":["research","hunter_io"],"target_field":"description"}]`)
	row := e.row(wsA, wb, 0, `{"company":"Globex"}`)
	cost := billing.ProjectedPlatformCost(1, platChain, nil)
	if cost <= 0 {
		t.Fatal("test premise: the platform chain must be platform billed")
	}
	return recEnv{rowsEnv: e, wb: wb, row: row, cost: cost}
}

func (e recEnv) recomputer(billingOn bool) *wbcrud.Recomputer {
	r := &wbcrud.Recomputer{BillingEnabled: billingOn}
	r.Run = runstart.Recompute(runstart.RecomputeDeps{
		BillingEnabled: billingOn,
		Config: func(_ context.Context, ws string) runplan.Config {
			return runplan.Config{WorkspaceID: ws, DefaultConcurrency: 12,
				Perf:     runplan.Perf{RowConcurrency: 12, MaxProviders: 8, RetryPasses: 1, ProviderWorkers: 8, ProviderTimeout: 10},
				Leads:    wbcrud.PGLeads{},
				NewRunID: func() string { return recomputeRunID }}
		},
	})
	return r
}

func (e recEnv) credit(usd float64) {
	e.t.Helper()
	if err := e.s.WithTx(e.ctx, wsA, func(tx pgx.Tx) error {
		_, err := billing.Credit(e.ctx, tx, wsA, usd, "seed", "rec-seed")
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e recEnv) start(r *wbcrud.Recomputer) (res *wbcrud.Obj) {
	e.t.Helper()
	err := e.s.WithTx(e.ctx, wsA, func(tx pgx.Tx) error {
		w, err := e.s.Owned(e.ctx, tx, wsA, e.wb, true)
		if err != nil {
			return err
		}
		var g *wbcrud.GuardError
		res, g, err = r.Start(e.ctx, tx, wsA, w, []any{"plat_x"}, []int64{e.row}, nil)
		if g != nil {
			e.t.Fatalf("guard %+v", g)
		}
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func (e recEnv) count(sql string, args ...any) (n int) {
	e.t.Helper()
	if err := e.s.WithTx(e.ctx, wsA, func(tx pgx.Tx) error { return tx.QueryRow(e.ctx, sql, args...).Scan(&n) }); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e recEnv) balance() (b float64) {
	e.t.Helper()
	if err := e.s.WithTx(e.ctx, wsA, func(tx pgx.Tx) (err error) { b, err = billing.Balance(e.ctx, tx, wsA); return err }); err != nil {
		e.t.Fatal(err)
	}
	return b
}

func TestRecomputeWithBillingDebitsOnce(t *testing.T) {
	e := newRecEnv(t)
	e.credit(10)
	r := e.recomputer(true)
	res := e.start(r)
	if st, _ := res.Get("status"); st != "started" {
		t.Fatalf("result %v", res)
	}
	if rid, _ := res.Get("run_id"); rid != recomputeRunID {
		t.Fatalf("run_id %v", rid)
	}
	if n := e.count(`SELECT count(*) FROM credit_ledger_entries WHERE run_id=$1 AND amount_usd < 0`, recomputeRunID); n != 1 {
		t.Fatalf("debit rows %d want 1", n)
	}
	if b := e.balance(); b != 10-e.cost {
		t.Fatalf("balance %v want %v", b, 10-e.cost)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE type='run_workbook' AND payload->>'run_id'=$1`, recomputeRunID); n != 1 {
		t.Fatalf("jobs %d", n)
	}
	// The same run id again (a retried recompute) is a replay: no second charge.
	e.start(r)
	if n := e.count(`SELECT count(*) FROM credit_ledger_entries WHERE run_id=$1 AND amount_usd < 0`, recomputeRunID); n != 1 {
		t.Fatalf("replay charged again: %d debit rows", n)
	}
	if b := e.balance(); b != 10-e.cost {
		t.Fatalf("replay changed the balance: %v", b)
	}
}

func TestRecomputeInsufficientCreditsResult(t *testing.T) {
	e := newRecEnv(t)
	e.credit(0.0001)
	res := e.start(e.recomputer(true))
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf) // the API's encoder: no HTML escaping, like FastAPI
	enc.SetEscapeHTML(false)
	if err := enc.Encode(res); err != nil {
		t.Fatal(err)
	}
	got := bytes.TrimRight(buf.Bytes(), "\n")
	// workbooks.py: HTTPException 402 -> {"status": "blocked", "detail": exc.detail}
	want := fmt.Sprintf(`{"status":"blocked","detail":{"message":%q,"balance_usd":0.0001,"required_usd":%s}}`,
		(&billing.InsufficientCreditsError{BalanceUSD: 0.0001, RequiredUSD: e.cost}).Error(), wbcrud.PyFloatRepr(e.cost))
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if n := e.count(`SELECT count(*) FROM credit_ledger_entries WHERE run_id=$1`, recomputeRunID); n != 0 {
		t.Fatalf("a blocked recompute wrote %d ledger rows", n)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE type='run_workbook'`); n != 0 {
		t.Fatalf("a blocked recompute enqueued %d jobs", n)
	}
	if n := e.count(`SELECT count(*) FROM workbooks WHERE id=$1 AND status='draft' AND last_run_at IS NULL`, e.wb); n != 1 {
		t.Fatal("a blocked recompute must not flip the workbook to running")
	}
}

func TestRecomputeBillingOffUnchanged(t *testing.T) {
	e := newRecEnv(t)
	res := e.start(e.recomputer(false)) // starter wired but billing off: plan 04 path, no ledger access
	if st, _ := res.Get("status"); st != "started" {
		t.Fatalf("result %v", res)
	}
	if n := e.count(`SELECT count(*) FROM credit_ledger_entries`); n != 0 {
		t.Fatalf("billing off wrote %d ledger rows", n)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE type='run_workbook' AND executor='python'`); n != 1 {
		t.Fatalf("want one python-executor job, got %d", n)
	}
}
