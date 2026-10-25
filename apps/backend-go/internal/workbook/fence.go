package workbook

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// ErrLeaseLost means this executor no longer owns the run_workbook job (reaped,
// reclaimed, cancelled or finished): the run must stop writing.
var ErrLeaseLost = errors.New("workbook: queue lease is no longer active")

// fence ports batch_attempts.fence_workbook_run_state/_owned_job: lock the job
// row iff (id, type run_workbook, workspace, worker_id, locked_at, lease_token,
// status processing) all still hold and its payload names this workbook. It
// must be the first statement of every transaction that writes run state, so
// the lock is held until commit.
func fence(ctx context.Context, tx pgx.Tx, c *queue.Claim, workbookID string) error {
	payload, err := gen.New(tx).LockOwnedRunWorkbookJob(ctx, gen.LockOwnedRunWorkbookJobParams{
		ID: int32(c.ID), WorkspaceID: c.WorkspaceID, WorkerID: c.WorkerID,
		LockedAt: pgtype.Timestamp{Time: c.LockedAt, Valid: true}, LeaseToken: c.LeaseToken,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	var p struct {
		WorkbookID any `json:"workbook_id"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return ErrLeaseLost
	}
	if wb, ok := p.WorkbookID.(string); !ok || wb != workbookID {
		return ErrLeaseLost
	}
	return nil
}

// checkOwner is check_workbook_run_owner: a preflight in its own short
// transaction so no lock is held across the provider call that follows.
func checkOwner(ctx context.Context, pool *pgxpool.Pool, c *queue.Claim, workbookID string) error {
	return tenancy.InWorkspace(ctx, pool, c.WorkspaceID, func(tx pgx.Tx) error {
		return fence(ctx, tx, c, workbookID)
	})
}
