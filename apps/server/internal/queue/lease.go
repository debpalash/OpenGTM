package queue

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrLeaseLost reports that a job attempt no longer owns its row: it was
// cancelled, timed out, or reclaimed by another worker.
var ErrLeaseLost = errors.New("queue: claim cancelled or lost")

const holdLeaseSQL = `
SELECT 1 FROM jobs
WHERE id = $1 AND worker_id = $2 AND locked_at = $3 AND status = 'processing'
FOR UPDATE`

// HoldLease locks the job row inside tx and fails with ErrLeaseLost unless
// this attempt still owns it. Handlers call it in the same transaction as
// their domain writes, so a stale attempt can never commit results after a
// cancellation or a newer claim; the row lock also orders the commit before
// any concurrent cancel or finalize.
func HoldLease(ctx context.Context, tx pgx.Tx, job Job) error {
	var one int
	err := tx.QueryRow(ctx, holdLeaseSQL, job.ID, job.Lease.WorkerID, job.Lease.LockedAt).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("queue: check lease for job %d: %w", job.ID, err)
	}
	return nil
}
