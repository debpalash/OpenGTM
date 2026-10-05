package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// ExecQuerier is satisfied by pgx.Tx.
type ExecQuerier interface {
	Querier
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

const enqueueOnceSQL = `
INSERT INTO jobs (type, payload, workspace_id, priority, fire_key, status,
                  created_at, next_run_at, max_retries, retry_count)
VALUES ($1, ($2::jsonb || jsonb_build_object('fire_key', $3::text))::json, NULL, 1, $3, 'pending',
        LOCALTIMESTAMP, $4::timestamptz::timestamp, 3, 0)
RETURNING id`

// EnqueueOnce is the Go counterpart of job_scheduling.enqueue_job_once: it
// takes the transaction-scoped advisory lock on hashtext(fire_key), returns
// created=false when an active (pending or processing) job already holds the
// key, and otherwise inserts a pending job due at runAt. Unlike Enqueue a
// duplicate is not an error and does not abort the transaction.
//
// Parity detail: enqueue_job_once builds the Job without a workspace_id, so
// scheduler-created jobs carry a NULL jobs.workspace_id (they bypass the
// per-workspace active cap). EnqueueOnce does the same; the workspace stays in
// the payload. payload must be a JSON object; fire_key is merged into it.
func EnqueueOnce(ctx context.Context, tx ExecQuerier, jobType string, payload any, fireKey string, runAt time.Time) (id int64, created bool, err error) {
	if jobType == "" || fireKey == "" {
		return 0, false, errors.New("queue: enqueue-once requires a job type and fire_key")
	}
	raw, err := encodePayload(payload)
	if err != nil {
		return 0, false, err
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil || probe == nil {
		return 0, false, errors.New("queue: payload must be a JSON object")
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", fireKey); err != nil {
		return 0, false, fmt.Errorf("queue: lock fire_key: %w", err)
	}
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM jobs WHERE fire_key = $1 AND status IN ('pending', 'processing'))`,
		fireKey).Scan(&exists); err != nil {
		return 0, false, fmt.Errorf("queue: check fire_key: %w", err)
	}
	if exists {
		return 0, false, nil
	}
	if err := tx.QueryRow(ctx, enqueueOnceSQL, jobType, raw, fireKey, runAt).Scan(&id); err != nil {
		return 0, false, fmt.Errorf("queue: enqueue %s once: %w", jobType, err)
	}
	return id, true, nil
}
