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

// HandBack returns a processing job to the Python executor (status pending,
// executor python, no retry spent) after a deterministic refusal. handedBack
// is false when the lease was lost; the caller then must not write anything.
func HandBack(ctx context.Context, pool *pgxpool.Pool, c *Claim, reason string) (handedBack bool, err error) {
	if r := []rune(reason); len(r) > 200 {
		reason = string(r[:200])
	}
	n, err := gen.New(pool).HandBackToPython(ctx, gen.HandBackToPythonParams{
		Error: "handed back to python: " + reason,
		ID:    int32(c.ID), WorkerID: c.WorkerID, LockedAt: c.lockedAt(), LeaseToken: c.LeaseToken,
	})
	if err != nil {
		return false, fmt.Errorf("queue: hand back: %w", err)
	}
	return n > 0, nil
}
