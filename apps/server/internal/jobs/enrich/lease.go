package enrich

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

const holdLeaseSharedSQL = `
SELECT 1 FROM jobs
WHERE id = $1 AND worker_id = $2 AND locked_at = $3 AND status = 'processing'
FOR SHARE`

// holdLease is queue.HoldLease with a shared row lock.
//
// A run commits dozens of cells at once from concurrent goroutines, and every
// one of them must prove the job lease in its own transaction. queue.HoldLease
// takes FOR UPDATE, which makes those transactions queue behind each other on
// the single job row and caps a run's throughput. The fencing guarantee does
// not need exclusivity between cells of one attempt, only against the writers
// that end the attempt: a cancellation, the queue's finalization, a timeout or
// a reclaim all UPDATE the row, and an UPDATE conflicts with FOR SHARE, so it
// waits until every cell transaction holding the lease has committed, and a
// transaction that starts after it sees the new state and fails with
// ErrLeaseLost. Cells of the same attempt do not conflict with each other.
func holdLease(ctx context.Context, tx pgx.Tx, job queue.Job) error {
	var one int
	err := tx.QueryRow(ctx, holdLeaseSharedSQL, job.ID, job.Lease.WorkerID, job.Lease.LockedAt).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return queue.ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("enrich: check lease for job %d: %w", job.ID, err)
	}
	return nil
}
