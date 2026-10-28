package queue

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// DefaultStaleAfter is how long a claim may go without a heartbeat before it counts as dead
// (QueueService.reap_dead_jobs_once: five minutes).
const DefaultStaleAfter = 5 * time.Minute

const reapReason = "Heartbeat Timeout"

// DefaultReapInterval is how often RunReaper looks for dead claims (Python's heartbeat monitor: every minute).
const DefaultReapInterval = time.Minute

// ReaperStaleAfter is the dead-claim threshold for workers that heartbeat every hb: DefaultStaleAfter, or three
// missed heartbeats when GO_WORKER_HEARTBEAT_SECONDS makes that longer, so a slow-but-alive worker is never reaped.
func ReaperStaleAfter(hb time.Duration) time.Duration {
	return max(DefaultStaleAfter, 3*hb)
}

// ReapStale recovers claims whose worker died (no heartbeat for staleAfter): a row with retries left goes back to
// pending with the normal 2^n minute backoff, otherwise it fails for good, exactly like the retry ceiling of a
// finished attempt. It is QueueService.reap_dead_jobs_once for every executor (no executor filter); the Go worker
// runs it (RunReaper) now that the Python reaper is gone. The rows stay locked for the transition (FOR UPDATE SKIP
// LOCKED), so replicas share the work and a row whose live owner is heartbeating or finalizing right now is skipped,
// not overwritten. The transition clears worker_id and locked_at, so a worker that comes back late is fenced: its
// heartbeat reports the lease lost and its finalize writes nothing. After the commit the failure hook of each job
// type mirrors the outcome into its domain ledger; a hook error never undoes the transition. It returns the number
// of recovered rows.
func ReapStale(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, staleAfter time.Duration) (int, error) {
	if staleAfter <= 0 {
		staleAfter = DefaultStaleAfter
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("queue: begin reap: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := gen.New(tx)
	rows, err := q.ReapCandidates(ctx, int32(staleAfter/time.Second))
	if err != nil {
		return 0, fmt.Errorf("queue: reap candidates: %w", err)
	}
	var notices []FailureNotice
	for _, r := range rows {
		retries := int(r.RetryCount.Int32)
		limit := defaultMaxRetries
		if r.MaxRetries.Valid {
			limit = int(r.MaxRetries.Int32)
		}
		willRetry := retries < limit
		var n int64
		if willRetry {
			n, err = q.ReapRequeue(ctx, gen.ReapRequeueParams{ID: r.ID, NewRetryCount: int32(retries + 1),
				BackoffMinutes: backoffMinutes(retries), Error: fmt.Sprintf("Retry %d: %s", retries+1, reapReason)})
		} else {
			n, err = q.ReapFail(ctx, gen.ReapFailParams{ID: r.ID, Error: "Final Failure: " + reapReason})
		}
		if err != nil {
			return 0, fmt.Errorf("queue: reap job %d: %w", r.ID, err)
		}
		if n > 0 {
			notices = append(notices, FailureNotice{JobID: int64(r.ID), JobType: r.Type.String, WorkspaceID: r.WorkspaceID.String,
				Payload: append([]byte(nil), r.Payload...), Reason: reapReason, WillRetry: willRetry})
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("queue: commit reap: %w", err)
	}
	for _, n := range notices {
		if log != nil {
			log.Warn("job claim recovered (heartbeat timeout)", "job_id", n.JobID, "job_type", n.JobType, "will_retry", n.WillRetry)
		}
		notifyFailure(ctx, log, n)
	}
	return len(notices), nil
}

// RunReaper calls ReapStale at once and then every interval until ctx ends. It must start after the job handlers
// registered their failure hooks (a recovered row whose hook is missing is not reconciled). An error is logged and
// the next tick tries again.
func RunReaper(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, interval, staleAfter time.Duration) {
	if log == nil {
		log = slog.Default()
	}
	if interval <= 0 {
		interval = DefaultReapInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := ReapStale(ctx, pool, log, staleAfter); err != nil && ctx.Err() == nil {
			log.Error("stale job reaper failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
