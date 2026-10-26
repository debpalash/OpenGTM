package retention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// DefaultBatchRows is the rows deleted per transaction (RETENTION_BATCH_ROWS).
const DefaultBatchRows = 5000

// Messages stored on a run, byte-equal to Python's.
const (
	errScheduledOff = "legal hold or scheduled retention disabled"
	errHoldMidRun   = "Legal hold enabled during retention enforcement"
	noMessage       = "retention enforcement failed"
)

// Deps is what enforcement and reconcile need.
type Deps struct {
	Pool *pgxpool.Pool
	// Now is the clock (default time.Now). The run's start, the cutoffs and the next
	// scheduled occurrence all derive from it.
	Now func() time.Time
	// BatchRows overrides RETENTION_BATCH_ROWS / DefaultBatchRows when > 0.
	BatchRows int
	// ExecutorFor picks "go" or "python" for the re-scheduled job (nil: python).
	ExecutorFor func(workspaceID string) string
	// AfterBatch, when set, runs after every committed delete batch (tests, metrics).
	AfterBatch func(category string, deleted int64)
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) batchRows() int {
	if d.BatchRows > 0 {
		return d.BatchRows
	}
	if n, err := strconv.Atoi(os.Getenv("RETENTION_BATCH_ROWS")); err == nil && n > 0 {
		return n
	}
	return DefaultBatchRows
}

func tstz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC().Truncate(time.Microsecond), Valid: true}
}

func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Enforce is handle_retention_enforce. A workspace without a policy is a no-op; a
// missing or unknown run id creates a "scheduler" run from the policy; completed and
// cancelled runs are left alone; a legal hold, or a scheduler run on a disabled
// policy, cancels the run and re-schedules. Otherwise the run starts and each target
// is deleted in batches of RETENTION_BATCH_ROWS, every batch in its own workspace
// transaction (RLS GUC and an explicit workspace predicate). Cutoffs are fixed once from
// the run's started_at. Before every batch the run and policy are re-read: a run
// cancelled meanwhile, or a legal hold set meanwhile, stops the job (the hold marks the
// run cancelled with the counts so far). Any error marks the run failed and is returned.
func Enforce(ctx context.Context, d Deps, workspaceID, runID string) error {
	if workspaceID == "" {
		return errors.New("retention enforcement requires workspace_id")
	}
	var (
		proceed bool
		run     gen.RetRunGetRow
	)
	err := tenancy.InWorkspace(ctx, d.Pool, workspaceID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		p, err := GetPolicy(ctx, tx, workspaceID)
		if err != nil || p == nil {
			return err
		}
		found := false
		if runID != "" {
			run, err = q.RetRunGet(ctx, gen.RetRunGetParams{ID: runID, WorkspaceID: workspaceID})
			if err == nil {
				found = true
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("retention: get run: %w", err)
			}
		}
		if !found {
			snap, err := snapshotOf(p)
			if err != nil {
				return err
			}
			id, err := q.RetRunCreate(ctx, gen.RetRunCreateParams{WorkspaceID: workspaceID, PolicySnapshot: snap})
			if err != nil {
				return fmt.Errorf("retention: create run: %w", err)
			}
			if run, err = q.RetRunGet(ctx, gen.RetRunGetParams{ID: id, WorkspaceID: workspaceID}); err != nil {
				return fmt.Errorf("retention: get run: %w", err)
			}
		}
		if run.Status == "completed" || run.Status == "cancelled" {
			return nil
		}
		now := d.now()
		if p.LegalHold || !p.Enabled && run.RequestedBy.String == "scheduler" && run.RequestedBy.Valid {
			if err := q.RetRunCancel(ctx, gen.RetRunCancelParams{ID: run.ID, WorkspaceID: workspaceID, Error: errScheduledOff, FinishedAt: tstz(now)}); err != nil {
				return fmt.Errorf("retention: cancel run: %w", err)
			}
			_, err := SchedulePolicy(ctx, tx, p, now, d.ExecutorFor)
			return err
		}
		start := now.UTC().Truncate(time.Microsecond)
		if err := q.RetRunStart(ctx, gen.RetRunStartParams{ID: run.ID, WorkspaceID: workspaceID, StartedAt: tstz(start)}); err != nil {
			return fmt.Errorf("retention: start run: %w", err)
		}
		run.StartedAt = tstz(start)
		proceed = true
		return nil
	})
	if err != nil || !proceed {
		return err
	}

	counts := Counts{}
	if err := d.deleteAll(ctx, workspaceID, run, counts); err != nil {
		if errors.Is(err, errStopped) {
			return nil
		}
		d.markFailed(ctx, workspaceID, run.ID, err, counts)
		return err
	}
	err = tenancy.InWorkspace(ctx, d.Pool, workspaceID, func(tx pgx.Tx) error {
		now := d.now()
		if err := finish(ctx, tx, workspaceID, run.ID, "completed", counts, nil, now); err != nil {
			return err
		}
		p, err := GetPolicy(ctx, tx, workspaceID)
		if err != nil || p == nil {
			return err
		}
		_, err = SchedulePolicy(ctx, tx, p, now, d.ExecutorFor)
		return err
	})
	if err != nil {
		d.markFailed(ctx, workspaceID, run.ID, err, counts)
	}
	return err
}

var errStopped = errors.New("retention: run stopped")

func snapshotOf(p *Policy) ([]byte, error) {
	var in map[string]any
	if len(p.RetentionDays) > 0 && string(p.RetentionDays) != "null" {
		if err := json.Unmarshal(p.RetentionDays, &in); err != nil {
			return nil, fmt.Errorf("retention: policy days: %w", err)
		}
	}
	days, err := NormalizeDays(in)
	if err != nil {
		return nil, err
	}
	return json.Marshal(days)
}

// snapshotDays reads the run's frozen policy. Python indexes the snapshot per
// category (a missing key fails the run); values outside the policy range are refused
// too so a corrupt snapshot can never widen what is deleted.
func snapshotDays(raw []byte) (map[string]int, error) {
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("retention: policy snapshot: %w", err)
	}
	out := map[string]int{}
	for _, c := range Categories {
		v, ok := in[c]
		if !ok {
			return nil, fmt.Errorf("retention: policy snapshot has no %q", c)
		}
		n, ok := asInt(v)
		if !ok || n < int64(MinDays[c]) || n > MaxDays {
			return nil, fmt.Errorf("retention: policy snapshot %s must be %d..%d days", c, MinDays[c], MaxDays)
		}
		out[c] = int(n)
	}
	return out, nil
}

func finish(ctx context.Context, tx pgx.Tx, ws, id, status string, counts Counts, errText *string, at time.Time) error {
	body, err := json.Marshal(counts)
	if err != nil {
		return err
	}
	p := gen.RetRunFinishParams{ID: id, WorkspaceID: ws, Status: status, DeletedCounts: body, FinishedAt: tstz(at)}
	if errText != nil {
		p.Error = pgtype.Text{String: *errText, Valid: true}
	}
	if err := gen.New(tx).RetRunFinish(ctx, p); err != nil {
		return fmt.Errorf("retention: finish run: %w", err)
	}
	return nil
}

func (d Deps) markFailed(ctx context.Context, ws, runID string, cause error, counts Counts) {
	msg := truncRunes(cause.Error(), 1000)
	ctx = context.WithoutCancel(ctx)
	_ = tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		return finish(ctx, tx, ws, runID, "failed", counts, &msg, d.now())
	})
}

func (d Deps) deleteAll(ctx context.Context, ws string, run gen.RetRunGetRow, counts Counts) error {
	days, err := snapshotDays(run.PolicySnapshot)
	if err != nil {
		return err
	}
	started := run.StartedAt.Time
	batch := d.batchRows()
	for _, t := range Targets() {
		if _, ok := counts[t.Category]; !ok {
			counts[t.Category] = 0
		}
		cut := Cutoff(t, days[t.Category], started)
		for {
			var n int64
			stopped := false
			err := tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
				q := gen.New(tx)
				st, err := q.RetRunLock(ctx, gen.RetRunLockParams{ID: run.ID, WorkspaceID: ws})
				if err != nil {
					return fmt.Errorf("retention: re-read run: %w", err)
				}
				var hold bool
				switch p, err := q.RetPolicyLockShared(ctx, ws); {
				case errors.Is(err, pgx.ErrNoRows):
				case err != nil:
					return fmt.Errorf("retention: re-read policy: %w", err)
				default:
					hold = p.LegalHold
				}
				if st.Status == "cancelled" {
					body, _ := json.Marshal(counts)
					if err := q.RetRunSetCounts(ctx, gen.RetRunSetCountsParams{ID: run.ID, WorkspaceID: ws, DeletedCounts: body}); err != nil {
						return err
					}
					stopped = true
					return nil
				}
				if hold {
					msg := errHoldMidRun
					if err := finish(ctx, tx, ws, run.ID, "cancelled", counts, &msg, d.now()); err != nil {
						return err
					}
					stopped = true
					return nil
				}
				n, err = deleteBatch(ctx, q, t, ws, cut, int32(batch))
				return err
			})
			if err != nil {
				return err
			}
			if stopped {
				return errStopped
			}
			counts[t.Category] += n
			if d.AfterBatch != nil {
				d.AfterBatch(t.Category, n)
			}
			if n < int64(batch) {
				break
			}
		}
	}
	return nil
}

func deleteBatch(ctx context.Context, q *gen.Queries, t Target, ws string, cut any, batch int32) (int64, error) {
	ts := func() pgtype.Timestamptz { return tstz(cut.(time.Time)) }
	var n int64
	var err error
	switch t.Table {
	case "governance_audit_events":
		n, err = q.RetDeleteAudit(ctx, gen.RetDeleteAuditParams{WorkspaceID: ws, Cutoff: ts(), Batch: batch})
	case "llm_usage_daily":
		n, err = q.RetDeleteLLMUsage(ctx, gen.RetDeleteLLMUsageParams{WorkspaceID: ws, Cutoff: cut.(string), Batch: batch})
	case "signals":
		n, err = q.RetDeleteSignals(ctx, gen.RetDeleteSignalsParams{WorkspaceID: ws, Cutoff: cut.(float64), Batch: batch})
	case "destination_deliveries":
		n, err = q.RetDeleteDeliveries(ctx, gen.RetDeleteDeliveriesParams{WorkspaceID: ws, Cutoff: ts(), Batch: batch})
	case "destination_inbound_receipts":
		n, err = q.RetDeleteInboundReceipts(ctx, gen.RetDeleteInboundReceiptsParams{WorkspaceID: ws, Cutoff: ts(), Batch: batch})
	case "audience_membership_events":
		n, err = q.RetDeleteMembershipEvents(ctx, gen.RetDeleteMembershipEventsParams{WorkspaceID: ws, Cutoff: ts(), Batch: batch})
	case "playbook_results":
		n, err = q.RetDeletePlaybookResults(ctx, gen.RetDeletePlaybookResultsParams{WorkspaceID: ws, Cutoff: ts(), Batch: batch})
	case "outreach_sends":
		n, err = q.RetDeleteOutreachSends(ctx, gen.RetDeleteOutreachSendsParams{WorkspaceID: ws, Cutoff: ts(), Batch: batch})
	default:
		err = fmt.Errorf("retention: unknown target %s", t.Table)
	}
	if err != nil {
		return 0, fmt.Errorf("retention: delete %s: %w", t.Table, err)
	}
	return n, nil
}
