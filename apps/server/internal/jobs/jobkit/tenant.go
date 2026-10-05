package jobkit

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/server/internal/db"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// WithLease runs fn in a transaction bound to workspaceID (forced row-level
// security) after confirming this attempt still owns the job: the lease row
// is locked first, so a cancelled, timed-out or reclaimed attempt fails with
// queue.ErrLeaseLost before it can write, and the lock orders the commit
// before any concurrent cancel or finalize.
//
// Handlers that must fence only the final commit of a long transaction call
// queue.HoldLease themselves just before their last write.
func WithLease(ctx context.Context, b db.Beginner, workspaceID string, job queue.Job, fn func(tx pgx.Tx) error) error {
	return db.WithTenant(ctx, b, workspaceID, func(tx pgx.Tx) error {
		if err := queue.HoldLease(ctx, tx, job); err != nil {
			return err
		}
		return fn(tx)
	})
}
