package queue

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Heartbeat renews the lease. active=false means the lease was lost (reaped,
// reclaimed, cancelled or finished) and the caller must cancel the job.
func Heartbeat(ctx context.Context, pool *pgxpool.Pool, c *Claim) (bool, error) {
	n, err := gen.New(pool).HeartbeatLease(ctx, gen.HeartbeatLeaseParams{
		ID: int32(c.ID), WorkerID: c.WorkerID, LockedAt: c.lockedAt(), LeaseToken: c.LeaseToken,
	})
	if err != nil {
		return false, fmt.Errorf("queue: heartbeat: %w", err)
	}
	return n > 0, nil
}

// IsCancelled reports whether the job was cancelled while we hold the lease.
// Callers throttle this to at most once per 2s per job.
func IsCancelled(ctx context.Context, pool *pgxpool.Pool, c *Claim) (bool, error) {
	ok, err := gen.New(pool).LeaseIsCancelled(ctx, gen.LeaseIsCancelledParams{
		ID: int32(c.ID), WorkerID: c.WorkerID, LockedAt: c.lockedAt(), LeaseToken: c.LeaseToken,
	})
	if err != nil {
		return false, fmt.Errorf("queue: cancel check: %w", err)
	}
	return ok, nil
}
