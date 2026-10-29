package queue

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Outcome is the result of running a job.
type Outcome struct {
	Status string // "completed" or "failed"
	Error  string
	// NoRetry sends a failed outcome straight to its final state (a
	// PermanentError from the handler).
	NoRetry bool
}

const defaultMaxRetries = 3

// Finalize ports the finalize block of QueueService._process_job. persisted is
// false when the lease was lost (nothing is written) or the job was cancelled
// (the committed cancelled status is preserved; only ownership is cleared).
func Finalize(ctx context.Context, pool *pgxpool.Pool, c *Claim, o Outcome) (persisted bool, err error) {
	persisted, _, err = FinalizeNotify(ctx, pool, c, o)
	return persisted, err
}

// FinalizeNotify is Finalize that also reports whether a persisted failed
// outcome was scheduled for retry (Python's will_retry), for failure hooks.
func FinalizeNotify(ctx context.Context, pool *pgxpool.Pool, c *Claim, o Outcome) (persisted, willRetry bool, err error) {
	if o.Status != "completed" && o.Status != "failed" {
		return false, false, fmt.Errorf("queue: invalid outcome status %q", o.Status)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, false, fmt.Errorf("queue: begin finalize: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := gen.New(tx)
	id, wid, la, tok := int32(c.ID), c.WorkerID, c.lockedAt(), c.LeaseToken

	row, err := q.LockLeaseForFinalize(ctx, gen.LockLeaseForFinalizeParams{ID: id, WorkerID: wid, LockedAt: la, LeaseToken: tok})
	if err == pgx.ErrNoRows {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("queue: lock lease: %w", err)
	}
	var n int64
	switch {
	case row.Status.String == "cancelled":
		n, err = q.FinalizeCancelled(ctx, gen.FinalizeCancelledParams{ID: id, WorkerID: wid, LockedAt: la, LeaseToken: tok})
		if err == nil && n > 0 {
			err = tx.Commit(ctx)
		}
		// A preserved cancellation is not a "persisted" outcome (Python does
		// not run failure handlers for it).
		return false, false, wrap(err)
	case o.Status == "completed":
		n, err = q.FinalizeCompleted(ctx, gen.FinalizeCompletedParams{ID: id, WorkerID: wid, LockedAt: la, LeaseToken: tok})
	default:
		retries := int(row.RetryCount.Int32)
		limit := defaultMaxRetries
		if row.MaxRetries.Valid {
			limit = int(row.MaxRetries.Int32)
		}
		if retries < limit && !o.NoRetry {
			willRetry = true
			// Python increments first, then backs off 2^(new-1) minutes,
			// i.e. 2^(retry_count before increment).
			n, err = q.FinalizeRetry(ctx, gen.FinalizeRetryParams{
				NewRetryCount: int32(retries + 1), BackoffMinutes: backoffMinutes(retries),
				Error: fmt.Sprintf("Retry %d: %s", retries+1, o.Error),
				ID:    id, WorkerID: wid, LockedAt: la, LeaseToken: tok,
			})
		} else {
			n, err = q.FinalizeFailed(ctx, gen.FinalizeFailedParams{
				Error: "Final Failure: " + o.Error, ID: id, WorkerID: wid, LockedAt: la, LeaseToken: tok,
			})
		}
	}
	if err != nil {
		return false, false, fmt.Errorf("queue: finalize: %w", err)
	}
	if n == 0 {
		return false, false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, false, fmt.Errorf("queue: commit finalize: %w", err)
	}
	return true, willRetry, nil
}

func wrap(err error) error {
	if err != nil {
		return fmt.Errorf("queue: finalize cancelled: %w", err)
	}
	return nil
}

// maxBackoffShift caps the exponent so a large retry_count cannot overflow
// int32 (2^30 minutes is already ~2000 years).
const maxBackoffShift = 30

// backoffMinutes is 2^retries minutes (retries before increment), capped.
func backoffMinutes(retries int) int32 {
	if retries > maxBackoffShift {
		retries = maxBackoffShift
	}
	if retries < 0 {
		retries = 0
	}
	return int32(1) << uint(retries)
}
