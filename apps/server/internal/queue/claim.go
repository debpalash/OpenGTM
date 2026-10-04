package queue

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// claimSQL is Python's _eligible_clause plus Go ownership: only registered
// types that job_executor_routes routes to 'go'. Ordering (priority, then the
// least-busy workspace, then due time and age) is identical so tenant
// fairness holds across a mixed fleet.
const claimSQL = `
SELECT j.id, j.workspace_id FROM jobs j
WHERE j.status = 'pending'
  AND (j.next_run_at IS NULL OR j.next_run_at <= LOCALTIMESTAMP)
  AND ($1::text IS NULL OR j.fire_key LIKE $1::text)
  AND (j.workspace_id IS NULL OR $2::int = 0 OR
       (SELECT COUNT(*) FROM jobs active WHERE active.status = 'processing'
          AND active.workspace_id = j.workspace_id) < $2::int)
  AND j.type = ANY($3::text[])
  AND EXISTS (SELECT 1 FROM job_executor_routes route
              WHERE route.job_type = j.type AND route.executor = 'go')
ORDER BY j.priority DESC,
  (SELECT COUNT(*) FROM jobs active WHERE active.status = 'processing'
     AND active.workspace_id = j.workspace_id) ASC,
  j.next_run_at ASC, j.created_at ASC
FOR UPDATE SKIP LOCKED LIMIT 1`

const claimUpdateSQL = `
UPDATE jobs SET status = 'processing', started_at = LOCALTIMESTAMP,
  last_heartbeat = LOCALTIMESTAMP, locked_at = LOCALTIMESTAMP, worker_id = $2
WHERE id = $1
RETURNING type, payload, coalesce(retry_count, 0), locked_at`

const maxClaimAttempts = 100

// Claim atomically takes the next eligible job for this worker, or returns
// nil when none is eligible.
func (q *Queue) Claim(ctx context.Context) (*Job, error) {
	types := q.Types()
	if len(types) == 0 {
		return nil, nil
	}
	var prefix *string
	if q.opts.FireKeyPrefix != "" {
		p := q.opts.FireKeyPrefix + "%"
		prefix = &p
	}
	for range maxClaimAttempts {
		job, atCap, err := q.claimOnce(ctx, types, prefix)
		if err != nil || !atCap {
			return job, err
		}
	}
	q.log.Warn("claim gave up after tenant-cap retries", "attempts", maxClaimAttempts)
	return nil, nil
}

// claimOnce reports atCap when the selected row's workspace filled up between
// the eligibility check and the advisory-locked recount; the caller retries.
func (q *Queue) claimOnce(ctx context.Context, types []string, prefix *string) (*Job, bool, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("queue: claim begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	capacity := q.opts.MaxActivePerWorkspace
	var id int64
	var workspace *string
	err = tx.QueryRow(ctx, claimSQL, prefix, capacity, types).Scan(&id, &workspace)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("queue: claim select: %w", err)
	}
	if workspace != nil && *workspace != "" && capacity > 0 {
		// The row lock prevents double claims of one job, but two replicas can
		// select different jobs of one workspace at once. The advisory lock
		// serialises the recount so the cap holds across replicas.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", *workspace); err != nil {
			return nil, false, fmt.Errorf("queue: claim tenant lock: %w", err)
		}
		var running int
		if err := tx.QueryRow(ctx,
			"SELECT COUNT(*) FROM jobs WHERE status = 'processing' AND workspace_id = $1",
			*workspace).Scan(&running); err != nil {
			return nil, false, fmt.Errorf("queue: claim tenant recount: %w", err)
		}
		if running >= capacity {
			return nil, true, nil
		}
	}
	job := Job{ID: id, Lease: Lease{WorkerID: q.opts.WorkerID}}
	if workspace != nil {
		job.WorkspaceID = *workspace
	}
	if err := tx.QueryRow(ctx, claimUpdateSQL, id, q.opts.WorkerID).
		Scan(&job.Type, &job.Payload, &job.RetryCount, &job.Lease.LockedAt); err != nil {
		return nil, false, fmt.Errorf("queue: claim update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("queue: claim commit: %w", err)
	}
	return &job, false, nil
}
