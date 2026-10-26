package autoengine

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoactions"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autocaps"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/billing"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// stats are the run counters of _run_one_action.
type stats struct {
	attempted, succeeded, skipped, failed int
	charged                               float64
}

// errAborted means the job's context ended while an action was in flight: the
// engine stops without recording the cancellation as a terminal result.
var errAborted = errors.New("autoengine: run aborted")

// errRollback rolls the prepare transaction back on purpose (a lost insert race
// or a credit refusal); it never leaves the package.
var errRollback = errors.New("autoengine: rollback")

// actionIn is everything one action of one row needs.
type actionIn struct {
	run     runInfo
	cand    candidate
	action  map[string]any
	idx     int
	dry     bool
	cells   map[string]any
	atype   string
	idem    string
	leadID  *int64
	leadSQL pgtype.Int4
}

type runInfo struct {
	ID, WorkspaceID, FireKey string
	Trigger                  gen.Trigger
	Caps                     autocaps.TriggerCaps
}

// prep is the outcome of the committed first phase of an action.
type prep struct {
	done        bool // nothing to execute: replay or cap refusal
	cap         bool // refused by a cap (attempted, then skipped)
	passed      bool // got past the replay check (Python counts it attempted from here)
	markerID    int32
	reservation autocaps.Reservation
	cost        float64
	raced       bool
	credits     bool
}

func isNonIdempotent(t string) bool { return autoactions.NonIdempotentTypes[t] }

// persistInsert inserts one result row; inserted is false when the idempotency
// key already exists.
func persistInsert(ctx context.Context, q *gen.Queries, a actionIn, status, skipReason, summary, errText string, hasSummary bool, charged float64) (id int32, inserted bool, err error) {
	p := gen.AutoRunInsertResultParams{
		WorkspaceID: a.run.WorkspaceID, RunID: a.run.ID, TriggerID: a.run.Trigger.ID, WorkbookID: a.cand.workbookID,
		RowID: a.cand.rowID, LeadID: a.leadSQL, ActionIndex: int32(a.idx), ActionType: a.atype,
		IdempotencyKey: a.idem, Status: status, ChargedUsd: charged,
	}
	if skipReason != "" {
		p.SkipReason = pgtype.Text{String: skipReason, Valid: true}
	}
	if hasSummary {
		p.ResultSummary = pgtype.Text{String: summary, Valid: true}
	}
	if errText != "" {
		p.Error = pgtype.Text{String: errText, Valid: true}
	}
	id, err = q.AutoRunInsertResult(ctx, p)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// prepare is the committed first phase of one action (everything before the
// external effect): replay check, then for a paid live action the cap
// reservation, the debit and the in_flight marker in ONE transaction (D6), so a
// crash can never charge without a marker or mark without the charge.
func (e *engine) prepare(ctx context.Context, tx pgx.Tx, a actionIn, existing *gen.AutoRunResultByIdemRow, pr *prep) error {
	q := gen.New(tx)
	ws := a.run.WorkspaceID
	cost := autoactions.ProjectCost(a.action, a.cand.cols, a.cand.defaults)
	paid := cost > 0
	if !paid {
		cost = 0
	}
	pr.cost = cost

	if paid && !a.dry {
		resv, err := autocaps.TryReserve(ctx, tx, a.run.Caps, ws, cost, a.idem, e.d.GlobalDailyUSD)
		if err != nil {
			return err
		}
		if !resv.OK {
			_, _, err := persistInsert(ctx, q, a, "skipped", "cap", "daily cap reached", "", true, 0)
			pr.done, pr.cap = true, true
			return err
		}
		pr.reservation = resv
		res, err := billing.CheckAndDebit(ctx, tx, e.d.BillingEnabled, ws, cost, a.idem, "automation_"+a.atype)
		var ins *billing.InsufficientCreditsError
		if errors.As(err, &ins) {
			// The reservation dies with this transaction (nothing was written
			// for the debit); the refusal is recorded in a fresh one.
			pr.credits = true
			return errRollback
		}
		if err != nil {
			return err
		}
		if res.IdempotentReplay {
			pr.cost = 0
		} else {
			pr.cost = res.ChargedUSD
		}
		if e.afterDebit != nil {
			if err := e.afterDebit(); err != nil {
				return err
			}
		}
	}

	if a.dry {
		return nil
	}
	if existing != nil {
		pr.markerID = existing.ID
		return q.AutoRunReuseMarker(ctx, gen.AutoRunReuseMarkerParams{ChargedUsd: pr.cost, ID: existing.ID, WorkspaceID: ws})
	}
	id, inserted, err := persistInsert(ctx, q, a, "in_flight", "", "", "", false, pr.cost)
	if err != nil {
		return err
	}
	if !inserted {
		pr.raced = true // a concurrent eval of the same key won; start over
		return errRollback
	}
	pr.markerID = id
	return nil
}

// runOne is _run_one_action. A returned error counts as a failed action (the
// run goes on); errAborted stops the whole run.
func (e *engine) runOne(ctx context.Context, a actionIn, st *stats) error {
	ws := a.run.WorkspaceID
	var pr prep
	for attempt := 0; ; attempt++ {
		pr = prep{}
		var existing *gen.AutoRunResultByIdemRow
		err := tenancy.InWorkspace(ctx, e.d.Pool, ws, func(tx pgx.Tx) error {
			pr = prep{}
			q := gen.New(tx)
			row, err := q.AutoRunResultByIdem(ctx, gen.AutoRunResultByIdemParams{WorkspaceID: ws, IdempotencyKey: a.idem})
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				existing = nil
			case err != nil:
				return err
			default:
				existing = &row
				switch row.Status.String {
				case "in_flight":
					if isNonIdempotent(a.atype) {
						// crash between debit and send: at most once, never re-sent
						if _, err := q.AutoRunMarkReplay(ctx, gen.AutoRunMarkReplayParams{ID: row.ID, WorkspaceID: ws}); err != nil {
							return err
						}
						pr.done = true
						return nil
					}
					// an idempotent action re-attempts on the same marker
				default: // success, skipped, charged_only, failed (and any unknown status)
					pr.done = true
					return nil
				}
			}
			pr.passed = true
			return e.prepare(ctx, tx, a, existing, &pr)
		})
		if errors.Is(err, errRollback) {
			err = nil
		}
		if err != nil {
			if pr.passed {
				st.attempted++ // _run_one_action counts the attempt before the reservation and debit
			}
			return err
		}
		if pr.raced && attempt < 3 {
			continue // re-read the winner's row
		}
		break
	}
	switch {
	case pr.done && pr.cap:
		st.attempted++
		st.skipped++
		return nil
	case pr.done:
		st.skipped++ // a replay is counted skipped, never attempted
		return nil
	case pr.raced:
		st.skipped++ // the winner of a concurrent insert owns the action
		return nil
	}
	st.attempted++
	if pr.credits {
		err := tenancy.InWorkspace(ctx, e.d.Pool, ws, func(tx pgx.Tx) error {
			_, _, err := persistInsert(ctx, gen.New(tx), a, "skipped", "credits", "insufficient credits", "", true, 0)
			return err
		})
		if err != nil {
			return err
		}
		st.skipped++
		return nil
	}
	return e.executeAndSettle(ctx, a, pr, st)
}

func (e *engine) executeAndSettle(ctx context.Context, a actionIn, pr prep, st *stats) error {
	ws := a.run.WorkspaceID
	res, err := e.execute(ctx, a)
	if err != nil {
		return err // a crash after the marker leaves it (and the reservation) as Python does
	}
	if a.dry {
		return nil // nothing is persisted or charged in a dry run
	}
	if cerr := ctx.Err(); cerr != nil && res.Status != "success" {
		// A shutdown or cancel is not the action's outcome: leave the marker
		// in_flight (an idempotent action re-attempts, a webhook is never
		// re-sent) and stop the run.
		return fmt.Errorf("%w: %w", errAborted, cerr)
	}
	err = tenancy.InWorkspace(ctx, e.d.Pool, ws, func(tx pgx.Tx) error {
		if pr.reservation.OK {
			var serr error
			if res.Status == "success" {
				serr = autocaps.Settle(ctx, tx, pr.reservation)
			} else {
				// The debit stays committed (refund-on-failure is out of scope);
				// only the reservation is freed.
				serr = autocaps.Release(ctx, tx, pr.reservation)
			}
			if serr != nil {
				return serr
			}
		}
		p := gen.AutoRunSettleResultParams{ID: pr.markerID, WorkspaceID: ws, Status: res.Status, ChargedUsd: pr.cost,
			ResultSummary: pgtype.Text{String: res.Summary, Valid: true}}
		if res.SkipReason != "" {
			p.SkipReason = pgtype.Text{String: res.SkipReason, Valid: true}
		}
		if res.Error != "" {
			p.Error = pgtype.Text{String: res.Error, Valid: true}
		}
		return gen.New(tx).AutoRunSettleResult(ctx, p)
	})
	if err != nil {
		return err
	}
	switch res.Status {
	case "success":
		st.succeeded++
		st.charged += pr.cost
	case "skipped":
		st.skipped++
	default:
		st.failed++
	}
	return nil
}

// execute runs the action and turns a panic into an error.
func (e *engine) execute(ctx context.Context, a actionIn) (res autoactions.Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("action panicked: %v", r)
		}
	}()
	run := e.d.Execute
	if run == nil {
		run = func(ctx context.Context, ws string, action map[string]any, wb, rowID string, leadID *int64,
			lead map[string]any, cols []autoactions.Column, idem string) autoactions.Result {
			return autoactions.Execute(ctx, e.d.Actions, ws, action, wb, rowID, leadID, lead, cols, idem)
		}
	}
	return run(ctx, a.run.WorkspaceID, a.action, a.cand.workbookID, a.cand.rowID, a.leadID, leadDataFor(a.cand.row), a.cand.cols, a.idem), nil
}
