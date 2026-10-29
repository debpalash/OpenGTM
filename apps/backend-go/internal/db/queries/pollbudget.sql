-- plan 08 (automations): poll budget ledger, watch_poll enqueue, watch_schedules
-- mirror and watch health writes. Tenant statements run inside tenancy.InWorkspace;
-- jobs and watch_schedules are non-RLS control tables.

-- name: PollDebitBudget :one
-- Atomic per-(workspace, UTC day) increment: the row lock taken by the upsert
-- serialises concurrent debits, so every caller sees a distinct n. A poll-now
-- debit bumps both counters (the shared budget and the manual quota).
INSERT INTO poll_budget_ledger (workspace_id, day, n, n_poll_now)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(day)::date, 1, sqlc.arg(poll_now)::int)
ON CONFLICT (workspace_id, day) DO UPDATE
SET n = poll_budget_ledger.n + 1,
    n_poll_now = poll_budget_ledger.n_poll_now + sqlc.arg(poll_now)::int
RETURNING n, n_poll_now;

-- name: PollNowQuotaUsed :one
SELECT COALESCE((SELECT n_poll_now FROM poll_budget_ledger
                 WHERE workspace_id = sqlc.arg(workspace_id)::text AND day = sqlc.arg(day)::date), 0)::int AS used;

-- name: PollActiveWatchJob :one
SELECT id FROM jobs
WHERE type = 'watch_poll' AND fire_key = sqlc.arg(fire_key)::text AND status IN ('pending', 'processing')
LIMIT 1;

-- name: PollInsertWatchJob :one
-- Same row as engine._enqueue_poll_if_absent (priority 1, max_retries 3); the
-- payload is final at insert (poll_now included). next_run_at is the
-- instant as naive UTC whatever the session TimeZone (R-tz).
INSERT INTO jobs (executor, type, payload, workspace_id, priority, fire_key, status,
                  created_at, next_run_at, max_retries, retry_count)
VALUES (sqlc.arg(executor)::text, 'watch_poll', sqlc.arg(payload)::json,
        NULLIF(sqlc.arg(workspace_id)::text, ''), 1, sqlc.arg(fire_key)::text, 'pending',
        (now() AT TIME ZONE 'UTC'), (sqlc.arg(next_run_at)::timestamptz AT TIME ZONE 'UTC'), 3, 0)
ON CONFLICT (fire_key) WHERE status IN ('pending', 'processing') DO NOTHING
RETURNING id;

-- name: PollMirrorUpsert :exec
INSERT INTO watch_schedules (watch_id, workspace_id, next_poll_at, enabled, updated_at)
VALUES (sqlc.arg(watch_id)::text, sqlc.arg(workspace_id)::text, sqlc.narg(next_poll_at)::timestamptz,
        sqlc.arg(enabled)::boolean, now())
ON CONFLICT (watch_id) DO UPDATE
SET workspace_id = EXCLUDED.workspace_id, next_poll_at = EXCLUDED.next_poll_at,
    enabled = EXCLUDED.enabled, updated_at = now();

-- name: PollMirrorDelete :exec
DELETE FROM watch_schedules WHERE watch_id = sqlc.arg(watch_id)::text;

-- name: PollLoadWatch :one
-- Row-locked: failure reconcile and a running poll both read-modify-write
-- consecutive_failures, so they serialise on the watch row.
SELECT id, workspace_id, "interval" AS poll_interval, schedule_anchor, enabled, next_poll_at,
       last_error, COALESCE(consecutive_failures, 0)::int AS consecutive_failures
FROM watch_subscriptions
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
FOR UPDATE;

-- name: PollSaveWatchHealth :exec
UPDATE watch_subscriptions
SET consecutive_failures = sqlc.arg(consecutive_failures)::int, enabled = sqlc.arg(enabled)::boolean,
    last_error = sqlc.narg(last_error)::text, next_poll_at = sqlc.narg(next_poll_at)::timestamptz
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: PollJobForReconcile :one
-- Python watch_poll jobs carry no jobs.workspace_id column value, only the payload's.
SELECT status, next_run_at FROM jobs
WHERE id = sqlc.arg(id)::bigint AND type = 'watch_poll'
  AND (workspace_id = sqlc.arg(workspace_id)::text
       OR (workspace_id IS NULL AND payload->>'workspace_id' = sqlc.arg(workspace_id)::text));

-- name: PollCancelPendingJob :exec
UPDATE jobs SET status = 'cancelled', error = sqlc.arg(error)::text, completed_at = (now() AT TIME ZONE 'UTC')
WHERE id = sqlc.arg(id)::bigint AND type = 'watch_poll'
  AND (workspace_id = sqlc.arg(workspace_id)::text
       OR (workspace_id IS NULL AND payload->>'workspace_id' = sqlc.arg(workspace_id)::text))
  AND status = 'pending';

-- name: PollDueMirror :many
-- Cross-tenant read of the non-RLS mirror (ids and timestamps only): bootstrap_watch_schedules.
SELECT watch_id, workspace_id FROM watch_schedules
WHERE enabled IS TRUE AND (next_poll_at IS NULL OR next_poll_at <= sqlc.arg(now)::timestamptz)
ORDER BY watch_id;
