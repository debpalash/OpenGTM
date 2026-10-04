package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Cancellation causes for an attempt's context.
var (
	errShutdown  = errors.New("queue: worker shutting down")
	errLeaseLost = ErrLeaseLost
	errJobDone   = errors.New("queue: attempt finished")
)

type timeoutError struct {
	id      int64
	jobType string
	limit   time.Duration
}

func (e *timeoutError) Error() string {
	// Same text as JobProcessTimeout so dashboards and greps keep working.
	return fmt.Sprintf("job %d (%s) exceeded %gs", e.id, e.jobType, e.limit.Seconds())
}

// dbTimeout bounds queue bookkeeping statements, which must not inherit the
// caller's cancellation: abandoning a claim or finalize commit mid-flight
// makes its outcome ambiguous and strands the row until the reaper.
const dbTimeout = 30 * time.Second

func dbContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
}

// Run recovers this worker's abandoned claims, starts the reaper and the
// claim slots, and blocks until ctx is cancelled. Shutdown stops new claims,
// lets running attempts finish within ShutdownGrace, then cancels them. An
// attempt cancelled by shutdown is not finalized; like the Python worker it
// is left for heartbeat recovery rather than falsely completed.
func (q *Queue) Run(ctx context.Context) error {
	q.warnUnrouted(ctx)
	if err := q.RecoverOwned(ctx); err != nil {
		q.log.Error("startup recovery failed", "err", err)
	}

	// Attempts outlive ctx during the drain, so they hang off their own root.
	attempts, stopAttempts := context.WithCancelCause(context.WithoutCancel(ctx))
	defer stopAttempts(errShutdown)

	reaperCtx, stopReaper := context.WithCancel(context.WithoutCancel(ctx))
	reaperDone := make(chan struct{})
	go func() {
		defer close(reaperDone)
		q.reapLoop(reaperCtx)
	}()

	q.log.Info("queue worker started", "slots", q.opts.Concurrency, "types", q.Types())
	var slots sync.WaitGroup
	for slot := range q.opts.Concurrency {
		slots.Go(func() { q.slotLoop(ctx, attempts, slot) })
	}

	<-ctx.Done()
	q.log.Info("draining queue slots", "grace", q.opts.ShutdownGrace, "active", q.active.Load())
	if !waitFor(&slots, q.opts.ShutdownGrace) {
		q.log.Warn("drain exceeded grace; cancelling active attempts", "active", q.active.Load())
		stopAttempts(errShutdown)
		if !waitFor(&slots, q.opts.HandlerStopGrace+dbTimeout) {
			q.log.Error("attempts did not stop after cancellation; exiting anyway")
		}
	}
	stopReaper()
	<-reaperDone
	q.log.Info("queue worker stopped")
	return nil
}

func waitFor(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (q *Queue) slotLoop(ctx, attempts context.Context, slot int) {
	for ctx.Err() == nil {
		claimCtx, cancel := dbContext(ctx)
		job, err := q.Claim(claimCtx)
		cancel()
		switch {
		case err != nil:
			q.log.Error("claim failed", "slot", slot, "err", err)
			sleepCtx(ctx, q.opts.ErrorBackoff)
		case job == nil:
			sleepCtx(ctx, q.opts.IdlePoll)
		default:
			q.active.Add(1)
			q.process(attempts, *job)
			q.active.Add(-1)
		}
	}
}

// process runs one attempt and records its outcome.
func (q *Queue) process(attempts context.Context, job Job) {
	log := q.log.With("job_id", job.ID, "job_type", job.Type)
	log.Info("processing job")

	jobCtx, cancelJob := context.WithCancelCause(attempts)
	defer cancelJob(errJobDone)
	limit := q.timeoutFor(job.Type)
	runCtx, cancelRun := context.WithTimeoutCause(jobCtx, limit,
		&timeoutError{id: job.ID, jobType: job.Type, limit: limit})
	defer cancelRun()

	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		q.monitor(jobCtx, job, cancelJob)
	}()

	result := make(chan error, 1)
	go func() { result <- q.invoke(runCtx, job) }()

	// cause is set only when the attempt was interrupted (timeout, lost
	// lease, shutdown); a handler that returned first keeps its own result.
	var herr, cause error
	select {
	case herr = <-result:
		if herr != nil {
			cause = context.Cause(runCtx)
		}
	case <-runCtx.Done():
		cause = context.Cause(runCtx)
		select {
		case herr = <-result:
		case <-time.After(q.opts.HandlerStopGrace):
			// Goroutines cannot be killed. Finalizing releases the lease, so
			// any later write the handler fences on it is rejected.
			log.Error("handler ignored cancellation; finalizing without it", "cause", cause)
		}
	}
	cancelJob(errJobDone)
	<-monitorDone

	var timeout *timeoutError
	failure := herr
	switch {
	case errors.Is(cause, errShutdown):
		log.Warn("attempt interrupted by shutdown; left for heartbeat recovery")
		return
	case errors.As(cause, &timeout):
		// Like Python's killed child: past the deadline the attempt failed,
		// whatever the handler reports afterwards.
		failure = timeout
	case errors.Is(cause, errLeaseLost):
		// Finalization will find the row cancelled (kept) or reclaimed
		// (untouched); this text only matters if neither holds.
		failure = fmt.Errorf("job %d (%s) claim cancelled or lost", job.ID, job.Type)
	}
	q.finish(job, failure, log)
}

// invoke calls the handler, converting a panic into an attempt failure.
func (q *Queue) invoke(ctx context.Context, job Job) (err error) {
	h := q.handler(job.Type)
	if h == nil {
		return fmt.Errorf("No handler for job type %s", job.Type)
	}
	defer func() {
		if r := recover(); r != nil {
			q.log.Error("handler panic", "job_id", job.ID, "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return h(ctx, job)
}

const claimActiveSQL = `
SELECT status = 'processing' FROM jobs
WHERE id = $1 AND worker_id = $2 AND locked_at = $3`

const renewSQL = `
UPDATE jobs SET last_heartbeat = LOCALTIMESTAMP
WHERE id = $1 AND worker_id = $2 AND locked_at = $3 AND status = 'processing'`

// monitor keeps the lease alive and cancels the attempt as soon as the row is
// cancelled or reclaimed, mirroring Python's 1s should_continue poll and 30s
// heartbeat.
func (q *Queue) monitor(ctx context.Context, job Job, cancel context.CancelCauseFunc) {
	check := time.NewTicker(q.opts.ClaimCheckInterval)
	defer check.Stop()
	beat := time.NewTicker(q.opts.HeartbeatInterval)
	defer beat.Stop()
	lease := job.Lease
	for {
		select {
		case <-ctx.Done():
			return
		case <-check.C:
			var active bool
			err := q.pool.QueryRow(ctx, claimActiveSQL, job.ID, lease.WorkerID, lease.LockedAt).Scan(&active)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && !active) {
				cancel(errLeaseLost)
				return
			}
			if err != nil && ctx.Err() == nil {
				q.log.Warn("claim check failed", "job_id", job.ID, "err", err)
			}
		case <-beat.C:
			tag, err := q.pool.Exec(ctx, renewSQL, job.ID, lease.WorkerID, lease.LockedAt)
			if err != nil {
				if ctx.Err() == nil {
					q.log.Warn("heartbeat failed", "job_id", job.ID, "err", err)
				}
				continue
			}
			if tag.RowsAffected() == 0 {
				cancel(errLeaseLost)
				return
			}
		}
	}
}

const finalizeSelectSQL = `
SELECT status, retry_count, max_retries, completed_at, next_run_at, error, LOCALTIMESTAMP
FROM jobs
WHERE id = $1 AND worker_id = $2 AND locked_at = $3 AND status IN ('processing', 'cancelled')
FOR UPDATE`

// The write repeats the lease and the observed status, as Python does, so it
// is safe even if the row lock were ever bypassed.
const finalizeUpdateSQL = `
UPDATE jobs SET worker_id = NULL, locked_at = NULL, status = $4, completed_at = $5,
  retry_count = $6, next_run_at = $7, error = $8
WHERE id = $1 AND worker_id = $2 AND locked_at = $3 AND status = $9`

type outcome struct {
	persisted, cancelled, willRetry bool
	reason                          string
}

// finalize records an attempt's result under its lease; failure nil means
// success.
func (q *Queue) finalize(ctx context.Context, job Job, failure error) (outcome, error) {
	var o outcome
	err := pgx.BeginFunc(ctx, q.pool, func(tx pgx.Tx) error {
		var (
			status                 string
			retryCount, maxRetries *int32
			completedAt, nextRunAt *time.Time
			errText                *string
			now                    time.Time
		)
		lease := job.Lease
		err := tx.QueryRow(ctx, finalizeSelectSQL, job.ID, lease.WorkerID, lease.LockedAt).
			Scan(&status, &retryCount, &maxRetries, &completedAt, &nextRunAt, &errText, &now)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // reclaimed or finalized elsewhere: never overwrite it
		}
		if err != nil {
			return err
		}
		previous := status
		switch {
		case status == "cancelled":
			// A committed cancellation is terminal; keep it.
			o.cancelled = true
			if completedAt == nil {
				completedAt = &now
			}
		case failure != nil:
			o.reason = failure.Error()
			retries := int32(0)
			if retryCount != nil {
				retries = *retryCount
			}
			limit := int32(3)
			if maxRetries != nil {
				limit = *maxRetries
			}
			if retries < limit {
				o.willRetry = true
				status = "pending"
				next := retries + 1
				retryCount = &next
				at := now.Add(backoff(int(next) - 1))
				nextRunAt = &at
				msg := fmt.Sprintf("Retry %d: %s", next, o.reason)
				errText = &msg
			} else {
				status = "failed"
				completedAt = &now
				msg := "Final Failure: " + o.reason
				errText = &msg
			}
		default:
			status = "completed"
			completedAt = &now
			errText = nil
		}
		tag, err := tx.Exec(ctx, finalizeUpdateSQL, job.ID, lease.WorkerID, lease.LockedAt,
			status, completedAt, retryCount, nextRunAt, errText, previous)
		if err != nil {
			return err
		}
		o.persisted = tag.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return outcome{}, fmt.Errorf("queue: finalize job %d: %w", job.ID, err)
	}
	return o, nil
}

// finish finalizes and then, only once the transition is committed, runs
// failure reconciliation, exactly as the Python parent worker does.
func (q *Queue) finish(job Job, failure error, log *slog.Logger) {
	ctx, cancel := dbContext(context.Background())
	defer cancel()
	o, err := q.finalize(ctx, job, failure)
	if err != nil {
		log.Error("failed to update job status", "err", err)
		return
	}
	switch {
	case !o.persisted:
		log.Info("attempt no longer owns the job; result discarded")
	case o.cancelled:
		log.Info("job was cancelled; cancellation preserved")
	case failure == nil:
		log.Info("job completed")
	case o.willRetry:
		log.Info("job failed; retry scheduled", "err", o.reason)
	default:
		log.Error("job failed permanently", "err", o.reason)
	}
	if failure == nil || !o.persisted || o.cancelled {
		return
	}
	q.reconcile(job, o.reason, o.willRetry)
}

func (q *Queue) reconcile(job Job, reason string, willRetry bool) {
	f := q.failure(job.Type)
	if f == nil {
		return
	}
	if reason == "" {
		reason = "job attempt failed"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*dbTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			q.log.Error("failure reconciliation panic", "job_id", job.ID, "panic", r)
		}
	}()
	if err := f(ctx, job, reason, willRetry); err != nil {
		q.log.Error("failure reconciliation failed", "job_id", job.ID, "job_type", job.Type, "err", err)
	}
}
