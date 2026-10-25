// Package queue is the Go side of the shared Postgres job queue. It mirrors
// legacy/python/apps/api/services/queue_service.py: every lease predicate is
// (id, worker_id, locked_at, lease_token, status), so Python and Go fence each
// other. Go never reaps; the Python reaper recovers stale claims of any executor.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

const (
	// JobTypeRunWorkbook is the only job type the Go executor runs.
	JobTypeRunWorkbook = "run_workbook"
	maxCapRetries      = 100
)

// Claim is a leased job.
type Claim struct {
	ID          int64
	Type        string
	WorkspaceID string
	Payload     json.RawMessage
	WorkerID    string
	LockedAt    time.Time
	LeaseToken  int64
}

func (c *Claim) lockedAt() pgtype.Timestamp {
	return pgtype.Timestamp{Time: c.LockedAt, Valid: true}
}

// ClaimNext claims the next eligible go-executor job, or returns (nil, nil).
// Go rows of unsupported types are failed explicitly and skipped.
func ClaimNext(ctx context.Context, pool *pgxpool.Pool, workerID string, tenantCap int) (*Claim, error) {
	for attempts := 0; attempts < maxCapRetries; {
		c, capRetry, err := claimOnce(ctx, pool, workerID, tenantCap)
		if err != nil {
			return nil, err
		}
		if capRetry {
			attempts++
			continue
		}
		if c == nil {
			return nil, nil
		}
		if c.Type != JobTypeRunWorkbook {
			// Each iteration removes one pending row, so this terminates.
			if _, err := gen.New(pool).FailUnsupported(ctx, gen.FailUnsupportedParams{
				ID: int32(c.ID), WorkerID: c.WorkerID, LockedAt: c.lockedAt(), LeaseToken: c.LeaseToken,
			}); err != nil {
				return nil, fmt.Errorf("queue: fail unsupported job %d: %w", c.ID, err)
			}
			continue
		}
		return c, nil
	}
	return nil, nil
}

// claimOnce runs one claim transaction. capRetry is true when the tenant cap
// recheck rejected the candidate.
func claimOnce(ctx context.Context, pool *pgxpool.Pool, workerID string, tenantCap int) (*Claim, bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("queue: begin claim: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := gen.New(tx)

	cand, err := q.SelectClaimCandidate(ctx, int32(tenantCap))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("queue: select candidate: %w", err)
	}
	if cand.WorkspaceID.Valid && cand.WorkspaceID.String != "" && tenantCap > 0 {
		// Serializes cap rechecks across replicas that picked different rows
		// for the same workspace before either claim was visible.
		if err := q.LockTenant(ctx, cand.WorkspaceID.String); err != nil {
			return nil, false, fmt.Errorf("queue: tenant lock: %w", err)
		}
		active, err := q.CountActiveForWorkspace(ctx, cand.WorkspaceID.String)
		if err != nil {
			return nil, false, fmt.Errorf("queue: count active: %w", err)
		}
		if active >= int64(tenantCap) {
			return nil, true, nil // deferred rollback releases the row lock
		}
	}
	row, err := q.MarkClaimed(ctx, gen.MarkClaimedParams{ID: cand.ID, WorkerID: workerID})
	if err != nil {
		return nil, false, fmt.Errorf("queue: mark claimed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("queue: commit claim: %w", err)
	}
	return &Claim{
		ID: int64(row.ID), Type: row.Type.String, WorkspaceID: row.WorkspaceID.String,
		Payload: json.RawMessage(row.Payload), WorkerID: row.WorkerID.String,
		LockedAt: row.LockedAt.Time, LeaseToken: row.LeaseToken,
	}, false, nil
}
