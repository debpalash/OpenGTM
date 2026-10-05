// Package audiencerefresh is the Go executor for the audience_refresh job
// type, a behavioural port of handle_audience_refresh,
// reconcile_audience_refresh_failure and schedule_next in
// apps/api/services/audiences/scheduler.py together with refresh_audience in
// audiences/refresh.py and the membership-event and destination-sync fan-out
// it triggers (automations/events.py emit_audience_membership,
// destinations/engine.py enqueue_audience_syncs).
//
// A refresh re-evaluates the audience's lead filter against the tenant's
// leads (the shared, RLS-protected PostgreSQL lead store), applies the diff to
// audience_members with durable entered/exited events, enqueues trigger_eval
// jobs for automation rules (when AUTOMATIONS_ENABLED) and a destination sync
// per enabled destination (both still executed by Python), records the
// audience's refresh health and books the next occurrence.
//
// The job type is Python-owned until an operator routes it:
//
//	opengtm routes set audience_refresh go      # cut over
//	opengtm routes set audience_refresh python  # roll back
//
// Not ported: the manual refresh API and bootstrap_audience_schedules stay in
// Python. See apps/server/README.md for the differences.
package audiencerefresh

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/db"
	"github.com/debpalash/OpenGTM/apps/server/internal/jobs/jobkit"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// JobType is the queue job type this package executes.
const JobType = "audience_refresh"

// PageSize is AUDIENCE_REFRESH_PAGE_SIZE: leads, members and events are
// processed in pages of this many rows.
const PageSize = 500

// maxErrorChars is the [:1000] truncation Python applies to recorded errors.
const maxErrorChars = 1000

// Interval bounds, in minutes: max(15, min(int(minutes or 60), 10080)).
const (
	defaultMinutes = 60
	minMinutes     = 15
	maxMinutes     = 10080
)

// Options configure a Worker.
type Options struct {
	// AutomationsEnabled mirrors the Python AUTOMATIONS_ENABLED setting. When
	// false (the default) no trigger_eval job is enqueued for membership
	// changes, as in emit_audience_membership.
	AutomationsEnabled bool
	// PageSize overrides PageSize (tests use small pages).
	PageSize int
}

// Worker executes audience_refresh jobs.
type Worker struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	opts Options

	// Now is the clock for member timestamps, refreshed_at and the next
	// occurrence. Python reads the process clock; tests freeze it.
	Now func() time.Time

	// beforeCommit runs inside the refresh transaction after all writes and
	// before the lease is re-checked (tests cancel the job here).
	beforeCommit func()
}

// NewWorker builds the job handler.
func NewWorker(pool *pgxpool.Pool, log *slog.Logger, opts Options) *Worker {
	if log == nil {
		log = slog.Default()
	}
	if opts.PageSize <= 0 {
		opts.PageSize = PageSize
	}
	return &Worker{pool: pool, log: log, opts: opts, Now: func() time.Time { return time.Now().UTC() }}
}

// Register adds the handler and its failure reconciler to a queue registry.
func (w *Worker) Register(r *queue.Registry) {
	r.Register(JobType, w.Handle)
	r.RegisterFailure(JobType, w.Reconcile)
}

type audience struct {
	id           string
	filters      string
	enabled      bool
	intervalMins *int64
	// nextSet reports a non-NULL next_refresh_at (the ORM always rewrites a
	// non-NULL aware value over a naive one, bumping updated_at).
	nextSet  bool
	failures int32
	// scheduled is true when next_refresh_at is in the future, reading the
	// stored naive value as UTC (Python's _as_utc), the reconciler's
	// "already reconciled" test.
	scheduled bool
}

func loadAudience(ctx context.Context, tx pgx.Tx, workspaceID, id string, now time.Time, forUpdate bool) (*audience, error) {
	sql := `SELECT id, filters::text, refresh_enabled, refresh_interval_minutes, next_refresh_at IS NOT NULL,
			coalesce(consecutive_refresh_failures, 0),
			coalesce((next_refresh_at AT TIME ZONE 'UTC') > $3::timestamptz, false)
		FROM audiences WHERE id = $1 AND workspace_id = $2`
	if forUpdate {
		sql += " FOR UPDATE"
	}
	var a audience
	err := tx.QueryRow(ctx, sql, id, workspaceID, now).
		Scan(&a.id, &a.filters, &a.enabled, &a.intervalMins, &a.nextSet, &a.failures, &a.scheduled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// refreshError tags a failure of the refresh itself (as opposed to the lease,
// cancellation or the bookkeeping around it): Python records those on the
// audience's health and still books the next occurrence.
type refreshError struct{ err error }

func (e *refreshError) Error() string { return e.err.Error() }
func (e *refreshError) Unwrap() error { return e.err }

// Handle runs one attempt of handle_audience_refresh.
//
// Python commits the member diff, then the events and syncs, then the health
// and the next occurrence in separate transactions, so a crash in between
// loses the fan-out of an already committed diff. Go does the whole refresh in
// one tenant transaction and fences its commit by the job lease: a cancelled
// or reclaimed attempt writes nothing, and a failure leaves no partial diff.
func (w *Worker) Handle(ctx context.Context, job queue.Job) error {
	payload, err := jobkit.PayloadObject(job.Payload)
	if err != nil {
		return err
	}
	workspaceID, audienceID := payload.Field("workspace_id"), payload.Field("audience_id")
	if workspaceID == "" || audienceID == "" {
		w.log.Warn("audience_refresh missing workspace_id/audience_id", "job_id", job.ID)
		return nil
	}
	log := w.log.With("job_id", job.ID, "workspace_id", workspaceID, "audience_id", audienceID)

	err = db.WithTenant(ctx, w.pool, workspaceID, func(tx pgx.Tx) error {
		now := w.Now().UTC()
		a, err := loadAudience(ctx, tx, workspaceID, audienceID, now, true)
		if err != nil {
			return err
		}
		if a == nil || !a.enabled {
			if err := removeSchedule(ctx, tx, workspaceID, audienceID); err != nil {
				return err
			}
			return queue.HoldLease(ctx, tx, job)
		}
		res, err := w.refresh(ctx, tx, workspaceID, a, now)
		if err != nil {
			return &refreshError{err}
		}
		if _, err := tx.Exec(ctx, `UPDATE audiences
			SET refresh_health = 'healthy', last_refresh_error = NULL, consecutive_refresh_failures = 0, updated_at = now()
			WHERE id = $1 AND workspace_id = $2`, a.id, workspaceID); err != nil {
			return err
		}
		if _, err := w.scheduleNext(ctx, tx, workspaceID, a, now); err != nil {
			return err
		}
		if w.beforeCommit != nil {
			w.beforeCommit()
		}
		// Fence the commit: a cancelled or reclaimed attempt rolls the whole
		// refresh back instead of publishing it.
		if err := queue.HoldLease(ctx, tx, job); err != nil {
			return err
		}
		log.Info("audience refreshed", "matched", res.matched, "entered", res.entered, "exited", res.exited, "changed", res.changed)
		return nil
	})
	var rf *refreshError
	switch {
	case err == nil:
		return nil
	case !errors.As(err, &rf), errors.Is(err, queue.ErrLeaseLost), ctx.Err() != nil:
		// A cancelled or reclaimed attempt writes nothing further; the queue
		// (reconciler) records the outcome of a timeout or retry.
		return err
	}

	// The refresh failed. A source failure must not strand a recurring
	// audience: record the health, book the next occurrence, then let the
	// queue retry or dead-letter the job.
	markErr := jobkit.WithLease(ctx, w.pool, workspaceID, job, func(tx pgx.Tx) error {
		now := w.Now().UTC()
		a, err := loadAudience(ctx, tx, workspaceID, audienceID, now, true)
		if err != nil || a == nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE audiences
			SET refresh_health = 'degraded', last_refresh_error = $3,
			    consecutive_refresh_failures = coalesce(consecutive_refresh_failures, 0) + 1, updated_at = now()
			WHERE id = $1 AND workspace_id = $2`, a.id, workspaceID,
			jobkit.TruncateChars(jobkit.Describe(rf.err), maxErrorChars)); err != nil {
			return err
		}
		a.failures++
		_, err = w.scheduleNext(ctx, tx, workspaceID, a, now)
		return err
	})
	if markErr != nil {
		log.Warn("could not record the failed audience refresh", "err", markErr)
	}
	return rf.err
}

// Reconcile ports reconcile_audience_refresh_failure. The queue calls it after
// committing a failed attempt (including timeouts and lost leases), with
// willRetry reporting whether the job went back to pending. A future
// next_refresh_at means the handler already recorded this attempt, which keeps
// a failure from being counted twice.
func (w *Worker) Reconcile(ctx context.Context, job queue.Job, reason string, willRetry bool) error {
	payload, err := jobkit.PayloadObject(job.Payload)
	if err != nil {
		return err
	}
	workspaceID := strings.TrimSpace(payload.Field("workspace_id"))
	audienceID := strings.TrimSpace(payload.Field("audience_id"))
	if workspaceID == "" || audienceID == "" {
		return errors.New("audience refresh failure payload requires workspace_id and audience_id")
	}
	if reason == "" {
		reason = "audience refresh failed"
	}
	now := w.Now().UTC()

	return db.WithTenant(ctx, w.pool, workspaceID, func(tx pgx.Tx) error {
		a, err := loadAudience(ctx, tx, workspaceID, audienceID, now, false)
		if err != nil {
			return err
		}
		if a == nil || !a.enabled {
			return nil
		}
		already := a.scheduled
		if !already {
			prefix := "Final failure"
			if willRetry {
				prefix = "Queue retry"
			}
			if _, err := tx.Exec(ctx, `UPDATE audiences
				SET refresh_health = 'degraded', last_refresh_error = $3,
				    consecutive_refresh_failures = coalesce(consecutive_refresh_failures, 0) + 1, updated_at = now()
				WHERE id = $1 AND workspace_id = $2`, a.id, workspaceID,
				jobkit.TruncateChars(prefix+": "+reason, maxErrorChars)); err != nil {
				return err
			}
			a.failures++
		}
		switch {
		case willRetry:
			if already {
				return nil
			}
			// Mirror the queue's next attempt without cancelling it.
			var retryAt *time.Time
			if err := tx.QueryRow(ctx, `SELECT next_run_at FROM jobs WHERE id = $1`, job.ID).Scan(&retryAt); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if retryAt == nil {
				return nil
			}
			if _, err := tx.Exec(ctx, `UPDATE audiences SET next_refresh_at = $3::timestamp, updated_at = now()
				WHERE id = $1 AND workspace_id = $2`, a.id, workspaceID, *retryAt); err != nil {
				return err
			}
			return mirror.UpsertNaive(ctx, tx, a.id, workspaceID, a.enabled, retryAt)
		case !already:
			_, err := w.scheduleNext(ctx, tx, workspaceID, a, now)
			return err
		}
		return nil
	})
}
