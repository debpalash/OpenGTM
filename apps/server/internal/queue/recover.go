package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Recovery only touches Go-routed types this worker can reconcile. The Python
// reaper owns everything else, and a Go reaper winning the compare-and-swap on
// a Python job would skip that job's Python failure handler.
const recoverSelectSQL = `
SELECT j.id, j.type, j.payload, j.workspace_id, j.worker_id, j.locked_at,
       j.last_heartbeat, j.retry_count, j.max_retries, LOCALTIMESTAMP
FROM jobs j
WHERE j.status = 'processing' AND %s`

const goOwnedSQL = `j.type = ANY($1::text[]) AND EXISTS (
  SELECT 1 FROM job_executor_routes route
  WHERE route.job_type = j.type AND route.executor = 'go')`

// staleWhere matches Python's reaper: no heartbeat for StaleAfter, or never.
const staleWhere = `(j.last_heartbeat < LOCALTIMESTAMP - make_interval(secs => $2::float8)
  OR j.last_heartbeat IS NULL) AND ` + goOwnedSQL

// ownedWhere is startup recovery. Unlike Python it does not adopt every
// unowned row: NULL-owner rows of Python types belong to the Python worker.
const ownedWhere = `(j.worker_id = $2 OR (j.worker_id IS NULL AND ` + goOwnedSQL + `))`

// The compare-and-swap repeats every field the selection observed. A
// concurrent heartbeat, cancellation, finalize or another reaper changes at
// least one of them, and then this update is a no-op.
const recoverCASSQL = `
UPDATE jobs SET status = $6, retry_count = $7, worker_id = NULL, locked_at = NULL,
  started_at = NULL, last_heartbeat = NULL, next_run_at = $8, completed_at = $9, error = $10
WHERE id = $1 AND status = 'processing'
  AND worker_id IS NOT DISTINCT FROM $2::varchar
  AND locked_at IS NOT DISTINCT FROM $3::timestamp
  AND last_heartbeat IS NOT DISTINCT FROM $4::timestamp
  AND retry_count IS NOT DISTINCT FROM $5::int`

type recovered struct {
	job       Job
	reason    string
	willRetry bool
}

// ReapOnce recovers stale Go-routed claims and returns how many it moved.
func (q *Queue) ReapOnce(ctx context.Context) (int, error) {
	return q.recoverAttempts(ctx, fmt.Sprintf(recoverSelectSQL, staleWhere),
		"Heartbeat Timeout", q.opts.StaleAfter.Seconds())
}

// RecoverOwned requeues claims a previous run of this worker identity left
// behind, plus unowned processing rows of Go-routed types.
func (q *Queue) RecoverOwned(ctx context.Context) error {
	_, err := q.recoverAttempts(ctx, fmt.Sprintf(recoverSelectSQL, ownedWhere),
		"Recovered from crash", q.opts.WorkerID)
	return err
}

func (q *Queue) reapLoop(ctx context.Context) {
	t := time.NewTicker(q.opts.ReapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := q.ReapOnce(ctx); err != nil {
				if ctx.Err() == nil {
					q.log.Error("reaper failed", "err", err)
				}
			} else if n > 0 {
				q.log.Warn("recovered jobs with expired heartbeats", "count", n)
			}
		}
	}
}

func (q *Queue) recoverAttempts(ctx context.Context, selectSQL, reason string, arg any) (int, error) {
	types := q.Types()
	if len(types) == 0 {
		return 0, nil
	}
	var done []recovered
	err := pgx.BeginFunc(ctx, q.pool, func(tx pgx.Tx) error {
		done = done[:0]
		rows, err := tx.Query(ctx, selectSQL, types, arg)
		if err != nil {
			return err
		}
		type candidate struct {
			job                    Job
			workerID               *string
			lockedAt, heartbeat    *time.Time
			retryCount, maxRetries *int32
			now                    time.Time
		}
		var found []candidate
		for rows.Next() {
			var c candidate
			var payload []byte
			var workspace *string
			if err := rows.Scan(&c.job.ID, &c.job.Type, &payload, &workspace, &c.workerID,
				&c.lockedAt, &c.heartbeat, &c.retryCount, &c.maxRetries, &c.now); err != nil {
				rows.Close()
				return err
			}
			c.job.Payload = json.RawMessage(payload)
			if workspace != nil {
				c.job.WorkspaceID = *workspace
			}
			found = append(found, c)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, c := range found {
			retries := int32(0)
			if c.retryCount != nil {
				retries = *c.retryCount
			}
			limit := int32(3)
			if c.maxRetries != nil {
				limit = *c.maxRetries
			}
			retry := retries < limit
			status, newRetries, msg := "failed", retries, "Final Failure: "+reason
			var nextRun, completed *time.Time
			if retry {
				status, newRetries, msg = "pending", retries+1, reason
				at := c.now.Add(backoff(int(retries)))
				nextRun = &at
			} else {
				completed = &c.now
			}
			tag, err := tx.Exec(ctx, recoverCASSQL, c.job.ID, c.workerID, c.lockedAt, c.heartbeat,
				c.retryCount, status, newRetries, nextRun, completed, msg)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 1 {
				c.job.RetryCount = int(newRetries)
				done = append(done, recovered{job: c.job, reason: reason, willRetry: retry})
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("queue: recover (%s): %w", reason, err)
	}
	for _, r := range done {
		q.log.Warn("recovered job", "job_id", r.job.ID, "job_type", r.job.Type,
			"reason", r.reason, "will_retry", r.willRetry)
		q.reconcile(r.job, r.reason, r.willRetry)
	}
	return len(done), nil
}

// warnUnrouted flags registered types that no route sends to Go; they would
// otherwise sit silently unclaimed.
func (q *Queue) warnUnrouted(ctx context.Context) {
	types := q.Types()
	if len(types) == 0 {
		q.log.Warn("no job handlers registered; this worker will not claim anything")
		return
	}
	rows, err := q.pool.Query(ctx, `
SELECT t FROM unnest($1::text[]) AS t
WHERE NOT EXISTS (SELECT 1 FROM job_executor_routes r WHERE r.job_type = t AND r.executor = 'go')`, types)
	if err != nil {
		q.log.Warn("could not check executor routes", "err", err)
		return
	}
	missing, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err == nil && len(missing) > 0 {
		q.log.Warn("registered job types are not routed to go and will not be claimed; "+
			"use `opengtm routes set <type> go` after draining Python workers", "types", missing)
	}
}
