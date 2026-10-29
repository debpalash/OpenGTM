package collectledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// ErrFailurePayload is reconcile_collect_job_failure's ValueError.
var ErrFailurePayload = errors.New("collect failure payload requires job_id, workspace_id, and slug")

// CheckFailurePayload mirrors the payload guard of reconcile_collect_job_failure: job_id,
// workspace_id and slug must all be non-empty (Python falsiness).
func CheckFailurePayload(jobID, workspaceID, slug string) error {
	if jobID == "" || slug == "" || workspaceID == "" {
		return ErrFailurePayload
	}
	return nil
}

// CollectFireKey is the durable-queue fire_key of a collection job.
func CollectFireKey(ws, id string) string { return "collect:" + ws + ":" + id }

// CancelQueueJob is _cancel_collection_queue_job: it cancels the pending/processing
// queue row of this collection job (status cancelled, completed_at = now as naive
// UTC, error 'Cancelled by user', worker_id and locked_at cleared) and reports the
// number of rows changed. ws must be the transaction's tenant.
func CancelQueueJob(ctx context.Context, tx pgx.Tx, ws, id string, now time.Time) (int, error) {
	if err := requireTenant(ctx, tx, ws); err != nil {
		return 0, err
	}
	n, err := gen.New(tx).CJCancelQueueJob(ctx, gen.CJCancelQueueJobParams{
		Now: pgtype.Timestamptz{Time: now, Valid: true}, FireKey: CollectFireKey(ws, id),
	})
	if err != nil {
		return 0, fmt.Errorf("collectledger: cancel queue job: %w", err)
	}
	return int(n), nil
}

// Reconcile is reconcile_collect_job_failure's ledger half: a parent-worker timeout or
// crash is mirrored into the job row. A missing or cancelled job is left alone; else a
// retry requeues the job ("Queue retry scheduled: <err>", completed_at cleared) and a
// final failure marks it failed ("Final failure: <err>", completed_at = now.isoformat()).
func Reconcile(ctx context.Context, tx pgx.Tx, id string, willRetry bool, errMsg string, now time.Time) error {
	q := gen.New(tx)
	row, err := q.CJGet(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("collectledger: reconcile status: %w", err)
	}
	if row.Status == "cancelled" {
		return nil
	}
	if willRetry {
		_, err = q.CJRequeueWithError(ctx, gen.CJRequeueWithErrorParams{ID: id, Error: "Queue retry scheduled: " + errMsg})
	} else {
		err = MarkEnqueueFailed(ctx, tx, id, "Final failure: "+errMsg, now)
	}
	if err != nil {
		return fmt.Errorf("collectledger: reconcile: %w", err)
	}
	return nil
}
