package runstart

import (
	"context"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/billing"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/runplan"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// RecomputeDeps wires the billing-aware recompute.
type RecomputeDeps struct {
	BillingEnabled bool
	// Config builds the run plan configuration for the workspace (executor
	// routing, perf settings, manifest index), as POST /run does.
	Config  func(ctx context.Context, ws string) runplan.Config
	Enqueue Enqueue // nil: EnqueueRunJob
}

// Recompute returns the wbcrud.RunStarter behind a row edit's reactive
// recompute under platform billing: workbooks.py calls run_workbook with
// force=True (row_columns for a bulk edit), so this plans, debits and enqueues
// exactly like POST /run, once per recompute (a fresh run id per call, the
// debit key scoped to the workspace). Insufficient credits is not an error:
// the row edit already stands, and Python answers
// {"status":"blocked","detail":{message,balance_usd,required_usd}} for the
// recompute. Other guards (422 cycle, 409 rows changed) are returned as guards.
func Recompute(d RecomputeDeps) wbcrud.RunStarter {
	return func(ctx context.Context, tx pgx.Tx, ws string, w wbcrud.Workbook, columnIDs []any, rowIDs []int64, rowColumns *wbcrud.Obj) (*wbcrud.Obj, *wbcrud.GuardError, error) {
		req := runplan.RunRequest{Force: true, RowIDs: rowIDs}
		req.ColumnIDs = make([]string, len(columnIDs))
		for i, id := range columnIDs {
			req.ColumnIDs[i] = wbcrud.PyStr(id)
		}
		if rowColumns != nil {
			req.RowColumns = map[string][]string{}
			for _, k := range rowColumns.Keys() {
				v, _ := rowColumns.Get(k)
				list, _ := v.([]any)
				ids := make([]string, len(list))
				for i, c := range list {
					ids[i] = wbcrud.PyStr(c)
				}
				req.RowColumns[k] = ids
			}
		}
		// A savepoint keeps the caller's transaction (which carries nothing but
		// this recompute) clean when the debit is refused.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return nil, nil, err
		}
		out, err := Start(ctx, sp, Input{WS: ws, Workbook: w, Req: req, Cfg: d.Config(ctx, ws), BillingEnabled: d.BillingEnabled, Enqueue: d.Enqueue})
		var ic *billing.InsufficientCreditsError
		var g *wbcrud.GuardError
		switch {
		case errors.As(err, &ic):
			if rerr := sp.Rollback(ctx); rerr != nil {
				return nil, nil, rerr
			}
			return Blocked(ic), nil, nil
		case errors.As(err, &g):
			_ = sp.Rollback(ctx)
			return nil, g, nil
		case err != nil:
			_ = sp.Rollback(ctx)
			return nil, nil, err
		}
		if err := sp.Commit(ctx); err != nil {
			return nil, nil, err
		}
		if out.Plan.Skipped {
			return wbcrud.RunResponse("skipped", 0, out.Plan.Message, nil, ""), nil, nil
		}
		return wbcrud.RunResponse("started", out.Plan.TotalJobs, out.Plan.Message, &out.JobID, out.Plan.RunID), nil, nil
	}
}

// Blocked is the recompute result for a 402: {"status":"blocked","detail":{...}}.
func Blocked(e *billing.InsufficientCreditsError) *wbcrud.Obj {
	detail := wbcrud.NewObj()
	detail.Set("message", e.Error())
	detail.Set("balance_usd", finite(e.BalanceUSD))
	detail.Set("required_usd", finite(e.RequiredUSD))
	o := wbcrud.NewObj()
	o.Set("status", "blocked")
	o.Set("detail", detail)
	return o
}

// finite clamps what JSON cannot carry (billing treats +Inf as insufficient).
func finite(f float64) float64 {
	switch {
	case math.IsNaN(f):
		return 0
	case math.IsInf(f, 1):
		return math.MaxFloat64
	case math.IsInf(f, -1):
		return -math.MaxFloat64
	}
	return f
}
