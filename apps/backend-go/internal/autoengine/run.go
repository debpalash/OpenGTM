// Package autoengine is the Go trigger_eval job handler (Python
// services/automations/engine.py handle_trigger_eval).
//
// Per eligible action the protocol is: replay check on the idempotency key;
// cap reservation, debit and the committed in_flight marker in one transaction
// (D6); the external effect only after the marker commits; then settle or
// release the reservation and the terminal update. Non-idempotent actions
// (webhook, send_email) are at-most-once across retries; idempotent ones
// (push_crm, re_enrich) re-attempt on the same marker.
package autoengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoactions"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autocaps"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autosvc"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/safecond"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// JobType is the queue job type this handler serves.
const JobType = "trigger_eval"

// ExecuteFunc runs one action (autoactions.Execute); Deps.Execute overrides it
// in tests.
type ExecuteFunc func(ctx context.Context, ws string, action map[string]any, workbookID, rowID string,
	leadID *int64, leadData map[string]any, cols []autoactions.Column, idem string) autoactions.Result

// Deps are the collaborators of the handler.
type Deps struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger

	Enabled        bool    // AUTOMATIONS_ENABLED: off makes the handler a no-op
	BillingEnabled bool    // BILLING_ENABLED
	MaxRowsPerEval int     // AUTOMATIONS_MAX_ROWS_PER_EVAL (default 500)
	GlobalDailyUSD float64 // AUTOMATIONS_GLOBAL_DAILY_USD (0 = unlimited)

	Actions autoactions.Deps
	Execute ExecuteFunc
	// ExecFor names the executor of a rescheduled eval ("go" or "python").
	ExecFor func(ws string) string
	// Emitter is the automation event emitter. The handler itself emits
	// nothing (Python's engine does not); re_enrich's run-end flush belongs to
	// the workbook run. The worker wiring shares one emitter through Deps.
	Emitter automationhook.Emitter
	// PyColumns reports that the worker has the Python column RPC, so a
	// re_enrich of ai_formula/research columns can run in Go. Agent columns and
	// every other non-Go type are handed back to Python.
	PyColumns bool
	// CheckEvery throttles the lease heartbeat and cancel probe between actions
	// (default 2s; negative probes before every action).
	CheckEvery time.Duration
	Now        func() time.Time
}

func (d Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

type engine struct {
	d          Deps
	afterDebit func() error // test seam: runs after the debit, before the marker
}

// Handle is handle_trigger_eval.
func Handle(ctx context.Context, d Deps, c *queue.Claim) error {
	return (&engine{d: d}).handle(ctx, c)
}

// pyBool is bool(v) of a decoded payload value.
func pyBool(v any) bool { return manifest.PyTruthy(v) }

func strOf(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	}
	return rowtmpl.PyStr(v)
}

func (e *engine) handle(ctx context.Context, c *queue.Claim) error {
	d := e.d
	if !d.Enabled {
		return nil
	}
	pl := map[string]any{}
	if c != nil && len(c.Payload) > 0 {
		pl = decodeObject(c.Payload)
	}
	ws, tid := strOf(pl["workspace_id"]), strOf(pl["trigger_id"])
	if ws == "" || tid == "" {
		d.log().Warn("trigger_eval missing workspace_id/trigger_id")
		return nil
	}
	dry := pyBool(pl["dry_run"])
	fireSource := "manual"
	if v, ok := pl["fire_source"]; ok {
		fireSource = strOf(v)
	}
	jobID := int64(0)
	if c != nil {
		jobID = c.ID
	}
	fireKey := strOf(pl["fire_key"])
	if fireKey == "" {
		fireKey = fmt.Sprintf("job:%d", jobID)
	}
	maxRows := d.MaxRowsPerEval
	if maxRows < 1 {
		maxRows = 500
	}

	var trig *gen.Trigger
	if err := tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		t, err := gen.New(tx).AutoRunTrigger(ctx, gen.AutoRunTriggerParams{ID: tid, WorkspaceID: ws})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		trig = &t
		return err
	}); err != nil {
		return err
	}

	var actions []map[string]any
	var scope []string
	if trig != nil {
		actions = parseActions(trig.Actions)
		scope = parseScope(trig.ScopeWorkbookIds)
		// Eligibility guard: decided before any write (D2, D8).
		reason, err := e.handBackReason(ctx, ws, actions, scope, pl["targets"], maxRows)
		if err != nil {
			return err
		}
		if reason != "" {
			return e.handBack(ctx, c, reason)
		}
	}

	if trig == nil || !trig.Enabled {
		// rule missing or paused: record a skipped run for observability
		return tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
			p := gen.AutoRunInsertRunParams{ID: autosvc.NewID(), WorkspaceID: ws, TriggerID: tid, JobID: int32(jobID),
				Status: "skipped", FireSource: fireSource, FireKey: fireKey,
				FinishedAt: pgtype.Timestamptz{Time: d.now(), Valid: true}}
			if trig != nil {
				p.TriggerNameSnapshot = pgtype.Text{String: trig.Name, Valid: true}
			}
			return gen.New(tx).AutoRunInsertRun(ctx, p)
		})
	}

	// The run row is committed before any debit.
	status := "running"
	if dry {
		status = "dry_run"
	}
	run := runInfo{ID: autosvc.NewID(), WorkspaceID: ws, FireKey: fireKey, Trigger: *trig, Caps: capsOf(*trig)}
	if err := tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		return gen.New(tx).AutoRunInsertRun(ctx, gen.AutoRunInsertRunParams{ID: run.ID, WorkspaceID: ws, TriggerID: trig.ID,
			TriggerNameSnapshot: pgtype.Text{String: trig.Name, Valid: true}, JobID: int32(jobID), Status: status,
			FireSource: fireSource, FireKey: fireKey})
	}); err != nil {
		return err
	}

	var cands []candidate
	if err := tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		var err error
		cands, err = resolveCandidates(ctx, tx, ws, scope, pl["targets"], maxRows)
		return err
	}); err != nil {
		return err
	}

	var st stats
	matched := 0
	g := &guard{d: d, c: c}
rows:
	for _, cand := range cands {
		cells := rowCells(cand.row)
		cond := ""
		if trig.Condition.Valid {
			cond = trig.Condition.String
		}
		if !safecond.Evaluate(cond, cells, cand.sc).Passed {
			continue
		}
		matched++
		var leadID *int64
		var leadSQL pgtype.Int4
		if cand.row.LeadID != nil {
			v := *cand.row.LeadID
			leadID = &v
			leadSQL = pgtype.Int4{Int32: int32(v), Valid: true}
		}
		for idx, action := range actions {
			if stop, err := g.check(ctx); err != nil || stop {
				if errors.Is(err, workbook.ErrLeaseLost) {
					return err // the new lease owner owns the run row
				}
				e.closeRun(ctx, run, dry, matched, st) // cancelled or errored: no reaper exists
				return err
			}
			err := e.runAction(ctx, actionIn{run: run, cand: cand, action: action, idx: idx, dry: dry, cells: cells,
				leadID: leadID, leadSQL: leadSQL}, &st)
			if err != nil {
				if errors.Is(err, errAborted) || ctx.Err() != nil {
					e.closeRun(ctx, run, dry, matched, st)
					return fmt.Errorf("trigger_eval aborted: %w", err)
				}
				d.log().Error("automation action failed", "trigger_id", trig.ID, "row_id", cand.rowID, "action_index", idx, "err", err)
				st.failed++
			}
			if trig.StopOnError && st.failed > 0 {
				break
			}
		}
		if trig.StopOnError && st.failed > 0 {
			break rows
		}
	}

	return e.finalize(ctx, run, fireSource, dry, matched, st, cands)
}

// closeRun finalises the run row of a cancelled or aborted sweep (status
// failed, counters so far, finished_at) on a detached context: the job context
// is the thing that ended. Nothing else runs (no activity rows, no reschedule).
// Best effort; the in_flight markers of unfinished actions are left alone.
func (e *engine) closeRun(ctx context.Context, run runInfo, dry bool, matched int, st stats) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	status := "failed"
	if dry {
		status = "dry_run"
	}
	err := tenancy.InWorkspace(cctx, e.d.Pool, run.WorkspaceID, func(tx pgx.Tx) error {
		_, err := gen.New(tx).AutoRunFinishRun(cctx, gen.AutoRunFinishRunParams{ID: run.ID, WorkspaceID: run.WorkspaceID, Status: status,
			MatchedRows: int32(matched), ActionsAttempted: int32(st.attempted), ActionsSucceeded: int32(st.succeeded),
			ActionsSkipped: int32(st.skipped), ActionsFailed: int32(st.failed), TotalChargedUsd: round4(st.charged)})
		return err
	})
	if err != nil {
		e.d.log().Warn("close aborted run failed", "run_id", run.ID, "err", fmt.Sprintf("%T", err))
	}
}

// runAction runs one action, turning a non-object action or an
// untyped one into an error (counted as failed).
func (e *engine) runAction(ctx context.Context, a actionIn, st *stats) error {
	if a.action == nil {
		return errors.New("action is not an object")
	}
	if t, ok := a.action["type"].(string); ok {
		a.atype = t
	} else if v, has := a.action["type"]; has && v != nil {
		a.atype = rowtmpl.PyStr(v)
	}
	a.idem = fmt.Sprintf("trig:%s:%s:%s:%s:%d:%s", a.run.WorkspaceID, a.run.Trigger.ID, a.cand.workbookID, a.cand.rowID, a.idx, a.run.FireKey)
	return e.runOne(ctx, a, st)
}

func capsOf(t gen.Trigger) autocaps.TriggerCaps {
	c := autocaps.TriggerCaps{ID: t.ID}
	if t.MaxActionsPerDay.Valid {
		v := t.MaxActionsPerDay.Int32
		c.MaxActionsPerDay = &v
	}
	if t.MaxSpendUsdPerDay.Valid {
		v := t.MaxSpendUsdPerDay.Float64
		c.MaxSpendUSDPerDay = &v
	}
	return c
}

// parseActions reads the rule's actions JSON in key order; elements that are
// not objects are nil (their action fails). A non-array value is no actions.
func parseActions(raw []byte) []map[string]any {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := make([]map[string]any, len(items))
	for i, it := range items {
		o, err := wbcrud.ParseObject(it)
		if err != nil {
			continue
		}
		m := map[string]any{}
		for _, k := range o.Keys() {
			m[k], _ = o.Get(k)
		}
		out[i] = m
	}
	return out
}

func parseScope(raw []byte) []string {
	var out []string
	var items []any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// guard throttles the lease heartbeat and cancel probe between actions.
type guard struct {
	d    Deps
	c    *queue.Claim
	last time.Time
}

// check heartbeats and probes for cancellation: stop is true after a cancel
// (the committed `cancelled` is never overwritten), err is workbook.ErrLeaseLost
// when the lease is gone.
func (g *guard) check(ctx context.Context) (stop bool, err error) {
	if g.c == nil {
		return false, ctx.Err()
	}
	every := g.d.CheckEvery
	if every == 0 {
		every = 2 * time.Second
	}
	if every > 0 && !g.last.IsZero() && time.Since(g.last) < every {
		return false, nil
	}
	g.last = time.Now()
	// A cancelled job is no longer active for the heartbeat, so probe it first.
	cancelled, err := queue.IsCancelled(ctx, g.d.Pool, g.c)
	if err != nil {
		return false, err
	}
	if cancelled {
		return true, nil
	}
	active, err := queue.Heartbeat(ctx, g.d.Pool, g.c)
	if err != nil {
		return false, err
	}
	if !active {
		return false, workbook.ErrLeaseLost
	}
	return false, nil
}

// handBack returns the job to Python (before any write) and reports it with the
// error the worker already understands.
func (e *engine) handBack(ctx context.Context, c *queue.Claim, reason string) error {
	if c == nil {
		return errors.New("autoengine: hand back needs a claim")
	}
	handed, err := queue.HandBack(ctx, e.d.Pool, c, reason)
	if err != nil {
		return err
	}
	if !handed {
		return workbook.ErrLeaseLost
	}
	e.d.log().Warn("trigger_eval handed back to python", "reason", reason)
	return fmt.Errorf("%w: %s", workbook.ErrHandedBack, reason)
}
