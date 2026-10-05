// Package retention is the Go executor for the retention_enforce job type.
//
// It is a behavioural port of apps/api/services/governance/retention.py
// (handle_retention_enforce and reconcile_retention_job_failure): the same
// status transitions, error strings, legal-hold and disabled-schedule
// handling, deleted_counts, and next-run scheduling. The Python code stays the
// source of truth; internal/jobs/retention parity tests (and
// tests/test_retention_go_parity_pg.py) run both on identical datasets and
// compare the results.
//
// The job type is Python-owned until an operator routes it:
//
//	opengtm routes set retention_enforce go      # cut over
//	opengtm routes set retention_enforce python  # roll back
//
// The API routes that create runs and enqueue jobs, and the scheduler's
// bootstrap_retention_schedules, remain in Python.
package retention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/db"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// JobType is the queue job type this package executes.
const JobType = "retention_enforce"

// DefaultBatchSize bounds the rows removed by one DELETE statement.
const DefaultBatchSize = 5000

// maxErrorChars is the [:1000] truncation Python applies to recorded errors.
const maxErrorChars = 1000

// Worker executes retention_enforce jobs.
type Worker struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	// Now is the clock for started_at/finished_at and the next-run schedule.
	// Python reads the process clock for all of them; tests freeze it.
	Now func() time.Time
	// BatchSize is the maximum rows per DELETE statement.
	BatchSize int

	// beforeFinish runs inside the purge transaction after the deletes and
	// before the lease is re-checked (tests cancel the job here).
	beforeFinish func()
}

// NewWorker builds the job handler.
func NewWorker(pool *pgxpool.Pool, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{pool: pool, log: log, Now: func() time.Time { return time.Now().UTC() }, BatchSize: DefaultBatchSize}
}

// Register adds the handler and its failure reconciler to a queue registry.
func (w *Worker) Register(r *queue.Registry) {
	r.Register(JobType, w.Handle)
	r.RegisterFailure(JobType, w.Reconcile)
}

// jobPayload holds the two payload fields retention reads.
type jobPayload struct {
	workspaceID string
	runID       string
}

// pyStr is `str(v) if v else ""` for a decoded payload value.
func pyStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		if truthy(t) {
			return string(t)
		}
	case bool:
		if t {
			return "True"
		}
	}
	return ""
}

func parsePayload(raw []byte) (jobPayload, error) {
	v, err := decodeValue(raw)
	if err != nil {
		return jobPayload{}, errBadPayload
	}
	obj, ok := v.(*object)
	if !ok {
		return jobPayload{}, errBadPayload
	}
	return jobPayload{workspaceID: pyStr(obj.vals["workspace_id"]), runID: pyStr(obj.vals["run_id"])}, nil
}

var errBadPayload = errors.New("retention payload must be a JSON object")

func truncateChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

type policyRow struct {
	enabled, legalHold bool
	retentionDays      string
}

type runRow struct {
	id          string
	status      string
	requestedBy *string
	snapshot    string
}

func loadPolicy(ctx context.Context, tx pgx.Tx, workspaceID string) (*policyRow, error) {
	var p policyRow
	err := tx.QueryRow(ctx,
		`SELECT enabled, legal_hold, retention_days::text FROM retention_policies WHERE workspace_id = $1`,
		workspaceID).Scan(&p.enabled, &p.legalHold, &p.retentionDays)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func loadRun(ctx context.Context, tx pgx.Tx, workspaceID, runID string) (*runRow, error) {
	if runID == "" {
		return nil, nil
	}
	var r runRow
	err := tx.QueryRow(ctx,
		`SELECT id, status, requested_by, policy_snapshot::text FROM retention_runs
		 WHERE id = $1 AND workspace_id = $2`, runID, workspaceID).
		Scan(&r.id, &r.status, &r.requestedBy, &r.snapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// createSchedulerRun inserts the pending run Python creates when a scheduled
// job arrives without a (valid) run_id.
func createSchedulerRun(ctx context.Context, tx pgx.Tx, workspaceID string, snapshot map[string]int64) (*runRow, error) {
	r := &runRow{status: "pending", snapshot: snapshotJSON(snapshot)}
	scheduler := "scheduler"
	r.requestedBy = &scheduler
	err := tx.QueryRow(ctx, `INSERT INTO retention_runs
		(id, workspace_id, status, requested_by, policy_snapshot, deleted_counts)
		VALUES (gen_random_uuid()::text, $1, 'pending', 'scheduler', $2::json, '{}'::json)
		RETURNING id`, workspaceID, r.snapshot).Scan(&r.id)
	return r, err
}

const stamp = "$%d::timestamptz::timestamp"

// Handle runs one attempt of handle_retention_enforce.
//
// Phases, each a tenant transaction under forced row-level security:
//  1. claim: validate, create the run if needed, and either cancel it
//     (legal hold / disabled schedule) or mark it running;
//  2. purge: delete expired rows in batches, then record completion and the
//     next scheduled run, all atomically and only while the lease is held;
//  3. on a purge failure, record the run as failed (lease permitting).
func (w *Worker) Handle(ctx context.Context, job queue.Job) error {
	pl, err := parsePayload(job.Payload)
	if err != nil {
		return err
	}
	if pl.workspaceID == "" {
		return errors.New("retention enforcement requires workspace_id")
	}
	log := w.log.With("job_id", job.ID, "workspace_id", pl.workspaceID)

	var (
		runID    string
		snapshot string
		started  time.Time
		proceed  bool
	)
	err = db.WithTenant(ctx, w.pool, pl.workspaceID, func(tx pgx.Tx) error {
		if err := queue.HoldLease(ctx, tx, job); err != nil {
			return err
		}
		policy, err := loadPolicy(ctx, tx, pl.workspaceID)
		if err != nil {
			return err
		}
		if policy == nil {
			return nil // nothing to enforce; Python returns silently too
		}
		run, err := loadRun(ctx, tx, pl.workspaceID, pl.runID)
		if err != nil {
			return err
		}
		if run == nil {
			days, err := normalizedDays([]byte(policy.retentionDays))
			if err != nil {
				return err
			}
			if run, err = createSchedulerRun(ctx, tx, pl.workspaceID, days); err != nil {
				return err
			}
		}
		if run.status == "completed" || run.status == "cancelled" {
			return nil
		}
		scheduled := run.requestedBy != nil && *run.requestedBy == "scheduler"
		if policy.legalHold || (!policy.enabled && scheduled) {
			if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE retention_runs
				SET status = 'cancelled', error = $2, finished_at = `+stamp+` WHERE id = $1`, 3),
				run.id, "legal hold or scheduled retention disabled", w.Now()); err != nil {
				return err
			}
			_, err := w.schedulePolicy(ctx, tx, pl.workspaceID, w.Now())
			return err
		}
		runID, proceed = run.id, true
		return tx.QueryRow(ctx, fmt.Sprintf(`UPDATE retention_runs
			SET status = 'running', started_at = `+stamp+` WHERE id = $1
			RETURNING started_at, policy_snapshot::text`, 2), run.id, w.Now()).Scan(&started, &snapshot)
	})
	if err != nil || !proceed {
		return err
	}

	err = w.purge(ctx, job, pl.workspaceID, runID, snapshot, started)
	var sched *scheduleError
	switch {
	case err == nil:
		log.Info("retention enforced", "run_id", runID)
		return nil
	case errors.Is(err, queue.ErrLeaseLost), ctx.Err() != nil, errors.As(err, &sched):
		// A cancelled or reclaimed attempt writes nothing further; the queue
		// (reconciler) records the outcome of a timeout or retry.
		return err
	}
	msg := truncateChars(err.Error(), maxErrorChars)
	markErr := db.WithTenant(ctx, w.pool, pl.workspaceID, func(tx pgx.Tx) error {
		if err := queue.HoldLease(ctx, tx, job); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE retention_runs
			SET status = 'failed', error = $2, finished_at = `+stamp+` WHERE id = $1`, 3),
			runID, msg, w.Now())
		return err
	})
	if markErr != nil {
		log.Warn("could not record failed retention run", "run_id", runID, "err", markErr)
	}
	return err
}

// scheduleError tags failures of the post-purge scheduling step. Python runs
// schedule_policy outside its try/except, so they never mark the run failed.
type scheduleError struct{ err error }

func (e *scheduleError) Error() string { return e.err.Error() }
func (e *scheduleError) Unwrap() error { return e.err }

// purge deletes expired rows and completes the run in one transaction.
func (w *Worker) purge(ctx context.Context, job queue.Job, workspaceID, runID, snapshotText string, started time.Time) error {
	snapshot, err := decodeValue([]byte(snapshotText))
	if err != nil {
		return fmt.Errorf("decode policy_snapshot: %w", err)
	}
	batch := w.BatchSize
	if batch <= 0 {
		batch = DefaultBatchSize
	}
	return db.WithTenant(ctx, w.pool, workspaceID, func(tx pgx.Tx) error {
		var order []string
		counts := map[string]int64{}
		for _, t := range targets {
			cutoff, err := cutoffFor(snapshot, t.category, started)
			if err != nil {
				return err
			}
			n, err := deleteBatches(ctx, tx, t, workspaceID, cutoff, batch)
			if err != nil {
				return err
			}
			if _, seen := counts[t.category]; !seen {
				order = append(order, t.category)
			}
			counts[t.category] += n
		}
		if w.beforeFinish != nil {
			w.beforeFinish()
		}
		// Fence the commit: a cancelled or reclaimed attempt rolls the
		// whole purge back instead of completing it.
		if err := queue.HoldLease(ctx, tx, job); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE retention_runs
			SET deleted_counts = $2::json, status = 'completed', finished_at = `+stamp+` WHERE id = $1`, 3),
			runID, countsJSON(order, counts), w.Now()); err != nil {
			return err
		}
		if _, err := w.schedulePolicy(ctx, tx, workspaceID, w.Now()); err != nil {
			return &scheduleError{err}
		}
		return nil
	})
}

// Reconcile ports reconcile_retention_job_failure. The queue calls it after
// committing a failed attempt (including timeouts and lost leases), with
// willRetry reporting whether the job went back to pending.
func (w *Worker) Reconcile(ctx context.Context, job queue.Job, reason string, willRetry bool) error {
	pl, err := parsePayload(job.Payload)
	if err != nil {
		return err
	}
	workspaceID := strings.TrimSpace(pl.workspaceID)
	if workspaceID == "" {
		return errors.New("retention failure payload requires workspace_id")
	}
	runID := strings.TrimSpace(pl.runID)
	if reason == "" {
		reason = "retention enforcement failed"
	}
	message := truncateChars(reason, maxErrorChars)
	now := w.Now()

	return db.WithTenant(ctx, w.pool, workspaceID, func(tx pgx.Tx) error {
		policy, err := loadPolicy(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		run, err := loadRun(ctx, tx, workspaceID, runID)
		if err != nil {
			return err
		}
		var jobStatus *string
		var jobFound bool
		if err := tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, job.ID).Scan(&jobStatus); err == nil {
			jobFound = true
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if run == nil {
			days := DefaultDays
			if policy != nil {
				if days, err = normalizedDays([]byte(policy.retentionDays)); err != nil {
					return err
				}
			}
			if run, err = createSchedulerRun(ctx, tx, workspaceID, days); err != nil {
				return err
			}
			if jobFound {
				if _, err := tx.Exec(ctx, `UPDATE jobs SET payload = (CASE
						WHEN jsonb_typeof(payload::jsonb) = 'object' THEN payload::jsonb
						ELSE '{}'::jsonb END || jsonb_build_object('run_id', $2::text))::json
					WHERE id = $1`, job.ID, run.id); err != nil {
					return err
				}
			}
		}
		if run.status == "completed" || run.status == "cancelled" {
			return nil
		}

		if policy != nil && policy.legalHold {
			const legalHold = "Legal hold enabled during retention enforcement"
			if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE retention_runs
				SET status = 'cancelled', error = $2, finished_at = `+stamp+` WHERE id = $1`, 3),
				run.id, legalHold, now); err != nil {
				return err
			}
			if jobFound && jobStatus != nil && *jobStatus == "pending" {
				_, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE jobs
					SET status = 'cancelled', error = $2, completed_at = `+stamp+` WHERE id = $1`, 3),
					job.ID, legalHold, now)
				return err
			}
			return nil
		}
		if willRetry {
			_, err := tx.Exec(ctx, `UPDATE retention_runs
				SET status = 'pending', error = $2, finished_at = NULL WHERE id = $1`,
				run.id, "Queue retry scheduled: "+message)
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE retention_runs
			SET status = 'failed', error = $2, finished_at = `+stamp+` WHERE id = $1`, 3),
			run.id, "Final failure: "+message, now); err != nil {
			return err
		}
		if policy != nil && policy.enabled {
			_, err := w.schedulePolicy(ctx, tx, workspaceID, now)
			return err
		}
		return nil
	})
}
