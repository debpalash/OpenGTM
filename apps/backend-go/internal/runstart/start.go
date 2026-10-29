// Package runstart is the one copy of "start a workbook run": plan, platform
// debit, workbook status flip, supersede of the in-flight run job and enqueue,
// all inside the caller's workspace transaction. POST /run and the reactive
// recompute after a row edit both go through it, so there is exactly one place
// that charges a run and a run is never charged twice.
package runstart

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/billing"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/runplan"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// DebitReason is check_and_debit's default reason.
const DebitReason = "run_debit"

// Enqueue inserts the run_workbook job with the planned executor inside tx.
type Enqueue func(ctx context.Context, tx pgx.Tx, ws, executor string, payload json.RawMessage) (int64, error)

// EnqueueRunJob is the production Enqueue.
func EnqueueRunJob(ctx context.Context, tx pgx.Tx, ws, executor string, payload json.RawMessage) (int64, error) {
	id, err := gen.New(tx).EnqueueRunWorkbookJob(ctx, gen.EnqueueRunWorkbookJobParams{
		Executor: executor, Payload: string(payload), WorkspaceID: ws,
	})
	return int64(id), err
}

// Input is one run start.
type Input struct {
	WS             string
	Workbook       wbcrud.Workbook
	Req            runplan.RunRequest
	Cfg            runplan.Config
	BillingEnabled bool
	Enqueue        Enqueue // nil: EnqueueRunJob
	// OnPlan sees a non-skipped plan before anything is written (audit metadata).
	OnPlan func(runplan.Plan)
}

// Outcome is the result of Start. Skipped plans wrote nothing.
type Outcome struct {
	Plan  runplan.Plan
	JobID int64 // zero when skipped
}

// Start plans the run and, unless it is skipped, writes it. The debit runs only
// with billing on and a positive projection (so an all-BYOK or free run never
// touches the ledger), keyed by the plan's run id, once per run id. Any error,
// including billing.InsufficientCreditsError (which writes nothing), is
// returned as is; the caller decides whether to roll tx back.
func Start(ctx context.Context, tx pgx.Tx, in Input) (Outcome, error) {
	plan, err := runplan.Build(ctx, tx, in.Workbook, in.Req, in.Cfg)
	if err != nil {
		return Outcome{}, err
	}
	if plan.Skipped {
		return Outcome{Plan: plan}, nil
	}
	if in.OnPlan != nil {
		in.OnPlan(plan)
	}
	if in.BillingEnabled && plan.ProjectedUSD > 0 {
		if _, err := billing.CheckAndDebit(ctx, tx, true, in.WS, plan.ProjectedUSD, plan.RunID, DebitReason); err != nil {
			return Outcome{}, err
		}
	}
	q := gen.New(tx)
	if err := q.StartWorkbookRun(ctx, gen.StartWorkbookRunParams{TotalRows: int32(plan.MatchedRows), WorkbookID: in.Workbook.ID, WorkspaceID: in.WS}); err != nil {
		return Outcome{}, err
	}
	if err := q.CancelActiveRunJobs(ctx, in.Workbook.ID); err != nil {
		return Outcome{}, err
	}
	enq := in.Enqueue
	if enq == nil {
		enq = EnqueueRunJob
	}
	jobID, err := enq(ctx, tx, in.WS, plan.Executor, plan.Payload)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Plan: plan, JobID: jobID}, nil
}
