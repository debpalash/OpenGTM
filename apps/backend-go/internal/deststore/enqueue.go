package deststore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

const (
	defaultFailure = "destination sync attempt failed"
	maxFailureLen  = 1000
)

type syncPayload struct {
	WorkspaceID string `json:"workspace_id"`
	RunID       string `json:"run_id"`
}

// FireKey is the single-flight key of a run's sync job.
func FireKey(runID string) string { return "destination_sync:" + runID }

// enqueueSync is the queue_service.add_job of a sync, as enqueue_job_once on the
// exact fire key (a payload of {workspace_id, run_id, fire_key}).
func enqueueSync(ctx context.Context, tx pgx.Tx, ws, runID, executor string) error {
	if _, _, err := queue.EnqueueOnce(ctx, tx, JobType, ws, syncPayload{WorkspaceID: ws, RunID: runID}, FireKey(runID), executor); err != nil {
		return fmt.Errorf("deststore: enqueue sync: %w", err)
	}
	return nil
}

// StartRun starts one sync of the destination, single-flight: under
// pg_advisory_xact_lock(hashtext('destsync:'||destID)) an existing pending, running
// or cancelling run is returned with created=false; otherwise a pending run and its
// audience_destination_sync job are written together (decision D7). The pair is
// atomic even when the caller commits after an error: the writes sit in a savepoint
// that a failed enqueue rolls back. wsID must be the transaction's tenant;
// ErrNotFound / ErrDisabled for an unknown or disabled destination; executor is
// "go" or "python"; requestedBy "" stores NULL.
func StartRun(ctx context.Context, tx pgx.Tx, wsID, destID, requestedBy, executor string) (Run, bool, error) {
	return startRun(ctx, tx, wsID, destID, requestedBy, func(Destination) string { return executor })
}

func startRun(ctx context.Context, tx pgx.Tx, wsID, destID, requestedBy string, executorFor func(Destination) string) (Run, bool, error) {
	if err := requireTenant(ctx, tx, wsID); err != nil {
		return Run{}, false, err
	}
	q := gen.New(tx)
	if err := q.DestLockSync(ctx, destID); err != nil {
		return Run{}, false, fmt.Errorf("deststore: lock sync: %w", err)
	}
	dest, err := getDestination(ctx, q, wsID, destID)
	if err != nil {
		return Run{}, false, err
	}
	if !dest.Enabled {
		return Run{}, false, ErrDisabled
	}
	if active, err := activeRun(ctx, q, wsID, destID); err != nil {
		return Run{}, false, err
	} else if active != nil {
		return *active, false, nil
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return Run{}, false, fmt.Errorf("deststore: savepoint: %w", err)
	}
	row, err := gen.New(sp).DestRunInsert(ctx, gen.DestRunInsertParams{
		ID: newUUID4(), WorkspaceID: wsID, DestinationID: destID,
		RequestedBy: pgtype.Text{String: requestedBy, Valid: requestedBy != ""},
	})
	if err == nil {
		err = enqueueSync(ctx, sp, wsID, row.ID, executorFor(dest))
	} else {
		err = fmt.Errorf("deststore: insert run: %w", err)
	}
	if err != nil {
		_ = sp.Rollback(ctx)
		return Run{}, false, err
	}
	if err := sp.Commit(ctx); err != nil {
		return Run{}, false, fmt.Errorf("deststore: release savepoint: %w", err)
	}
	return runFrom(row), true, nil
}

// EnqueueAudienceSyncs is enqueue_audience_syncs: one sync (requested_by
// "audience_refresh") for every enabled destination of the audience that has no
// active run; it returns how many were started. executorFor picks the job's
// executor per destination type and is only asked when a sync is actually started
// (nil: python).
func EnqueueAudienceSyncs(ctx context.Context, tx pgx.Tx, wsID, audienceID string, executorFor func(destType string) string) (int, error) {
	if err := requireTenant(ctx, tx, wsID); err != nil {
		return 0, err
	}
	dests, err := gen.New(tx).DestEnabledForAudience(ctx, gen.DestEnabledForAudienceParams{WorkspaceID: wsID, AudienceID: audienceID})
	if err != nil {
		return 0, fmt.Errorf("deststore: list enabled destinations: %w", err)
	}
	pick := func(d Destination) string {
		if executorFor == nil {
			return "python"
		}
		return executorFor(d.Type)
	}
	n := 0
	for _, d := range dests {
		_, created, err := startRun(ctx, tx, wsID, d.ID, "audience_refresh", pick)
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrDisabled) { // changed since the listing
			continue
		}
		if err != nil {
			return n, err
		}
		if created {
			n++
		}
	}
	return n, nil
}

// RetryRun is retry_run: a failed or completed_with_errors run goes back to pending
// (error and finish time cleared, counters kept so successful idempotency keys skip)
// and its sync job is enqueued again on the run's fire key. ErrRunNotFound,
// ErrNotRetryable, ErrDisabled as the Python route answers; the state change and the
// job are atomic.
func RetryRun(ctx context.Context, tx pgx.Tx, runID string, executorFor func(destType string) string) (Run, error) {
	ws, err := tenant(ctx, tx)
	if err != nil {
		return Run{}, err
	}
	q := gen.New(tx)
	cur, err := lockRun(ctx, q, ws, runID)
	if err != nil {
		return Run{}, err
	}
	if cur.Status != "failed" && cur.Status != "completed_with_errors" {
		return Run{}, ErrNotRetryable
	}
	dest, err := getDestination(ctx, q, ws, cur.DestinationID)
	if err != nil {
		return Run{}, err
	}
	if !dest.Enabled {
		return Run{}, ErrDisabled
	}
	executor := "python"
	if executorFor != nil {
		executor = executorFor(dest.Type)
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return Run{}, fmt.Errorf("deststore: savepoint: %w", err)
	}
	row, err := gen.New(sp).DestRunSetPending(ctx, gen.DestRunSetPendingParams{WorkspaceID: ws, ID: cur.ID})
	if err == nil {
		err = enqueueSync(ctx, sp, ws, cur.ID, executor)
	} else {
		err = fmt.Errorf("deststore: reset run: %w", err)
	}
	if err != nil {
		_ = sp.Rollback(ctx)
		return Run{}, err
	}
	if err := sp.Commit(ctx); err != nil {
		return Run{}, fmt.Errorf("deststore: release savepoint: %w", err)
	}
	return runFrom(row), nil
}

// ReconcileFailure is reconcile_destination_job_failure: it mirrors a killed or
// failed queue attempt into the run so a dead worker cannot leave a running run
// that blocks the destination. run.WorkspaceID must be the transaction's tenant and
// run.ID names the run (it is re-read under a row lock; a missing run, or one that
// is completed, completed_with_errors or cancelled, is left alone). A cancelling run
// finishes cancelled. Otherwise, with willRetry the run returns to pending ("Queue
// retry scheduled: ...") and in-flight deliveries to pending; without it the run
// fails ("Final failure: ...", message cut to 1000 characters), open deliveries
// fail, counts are recomputed and the destination becomes degraded.
func ReconcileFailure(ctx context.Context, tx pgx.Tx, run Run, willRetry bool, errText string) error {
	if err := requireTenant(ctx, tx, run.WorkspaceID); err != nil {
		return err
	}
	ws := run.WorkspaceID
	q := gen.New(tx)
	cur, err := lockRun(ctx, q, ws, run.ID)
	if errors.Is(err, ErrRunNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	switch cur.Status {
	case "completed", "completed_with_errors", "cancelled":
		return nil
	}
	if cur.Status == "cancelling" {
		dest, err := destinationOrNil(ctx, q, ws, cur.DestinationID)
		if err != nil {
			return err
		}
		_, err = finishCancelled(ctx, q, ws, cur.ID, dest)
		return err
	}
	if errText == "" {
		errText = defaultFailure
	}
	msg := truncRunes(errText, maxFailureLen)
	if willRetry {
		text := "Queue retry scheduled: " + msg
		if _, err := q.DestRunSetPending(ctx, gen.DestRunSetPendingParams{WorkspaceID: ws, ID: cur.ID, Error: pgtype.Text{String: text, Valid: true}}); err != nil {
			return fmt.Errorf("deststore: reconcile retry: %w", err)
		}
		if err := q.DestDeliveriesResetInFlight(ctx, gen.DestDeliveriesResetInFlightParams{WorkspaceID: ws, RunID: cur.ID, Error: text}); err != nil {
			return fmt.Errorf("deststore: reconcile retry deliveries: %w", err)
		}
		return nil
	}
	text := "Final failure: " + msg
	if err := q.DestDeliveriesFailOpen(ctx, gen.DestDeliveriesFailOpenParams{WorkspaceID: ws, RunID: cur.ID, Error: text}); err != nil {
		return fmt.Errorf("deststore: reconcile fail deliveries: %w", err)
	}
	counts, err := countDeliveries(ctx, q, ws, cur.ID)
	if err != nil {
		return err
	}
	if _, err := q.DestRunSetFinished(ctx, gen.DestRunSetFinishedParams{
		Status: "failed", Error: text, WorkspaceID: ws, ID: cur.ID,
		Attempted: int32(counts.Attempted), Succeeded: int32(counts.Succeeded), Failed: int32(counts.Failed), Skipped: int32(counts.Skipped),
	}); err != nil {
		return fmt.Errorf("deststore: reconcile fail run: %w", err)
	}
	if err := q.DestMarkDegraded(ctx, gen.DestMarkDegradedParams{WorkspaceID: ws, ID: cur.DestinationID, LastError: text}); err != nil {
		return fmt.Errorf("deststore: reconcile degrade destination: %w", err)
	}
	return nil
}
