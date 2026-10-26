-- plan 10 (audiences): schedule_next, the audience_schedules mirror and the failure
-- reconcile. audiences is RLS (run inside tenancy.InWorkspace, with the explicit
-- workspace filter as well); audience_schedules and jobs are non-RLS control tables.
-- Timestamps are naive UTC columns; instants bind as timestamptz and cast to
-- timestamp exactly as the autoschedule/retention mirrors do.

-- name: AudSchedLockAudience :one
SELECT refresh_enabled, refresh_interval_minutes, next_refresh_at
FROM audiences
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
FOR UPDATE;

-- Python cancels with fire_key LIKE 'audience_refresh:<id>:%', where '_' and '%' in an id
-- are wildcards and 'a' also matches the key of audience 'a:b'. Here: exact prefix, the
-- remainder must be an occurrence (ISO timestamp or the bootstrap marker), and a payload
-- that names a workspace must name this transaction's tenant.
-- name: AudSchedCancelPending :execrows
UPDATE jobs SET status = 'cancelled'
WHERE type = 'audience_refresh' AND status = 'pending'
  AND starts_with(fire_key, sqlc.arg(fire_key_prefix)::text)
  AND substr(fire_key, char_length(sqlc.arg(fire_key_prefix)::text) + 1) ~ '^(bootstrap$|[0-9]{4}-[0-9]{2}-[0-9]{2}T)'
  AND (payload::jsonb ->> 'workspace_id' IS NULL
       OR payload::jsonb ->> 'workspace_id' = coalesce(current_setting('app.workspace_id', true), ''));

-- Row enqueue_job_once writes: priority 1, max_retries 3, next_run_at = the occurrence,
-- workspace_id NULL as in Python.
-- name: AudSchedInsertJob :one
INSERT INTO jobs (executor, type, payload, priority, fire_key, status, created_at, next_run_at, max_retries, retry_count)
VALUES (sqlc.arg(executor)::text, 'audience_refresh', sqlc.arg(payload)::json, 1, sqlc.arg(fire_key)::text, 'pending',
        (now() AT TIME ZONE 'UTC'), (sqlc.arg(next_run_at)::timestamptz)::timestamp, 3, 0)
ON CONFLICT (fire_key) WHERE status IN ('pending', 'processing') DO NOTHING
RETURNING id;

-- SQLAlchemy only writes a changed attribute (and then bumps updated_at).
-- name: AudSchedSetNextRefresh :exec
UPDATE audiences
SET next_refresh_at = (sqlc.narg(next_refresh_at)::timestamptz)::timestamp, updated_at = now()
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
  AND next_refresh_at IS DISTINCT FROM (sqlc.narg(next_refresh_at)::timestamptz)::timestamp;

-- name: AudSchedSetNextRefreshNaive :exec
UPDATE audiences
SET next_refresh_at = sqlc.narg(next_refresh_at)::timestamp, updated_at = now()
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
  AND next_refresh_at IS DISTINCT FROM sqlc.narg(next_refresh_at)::timestamp;

-- name: AudSchedUpsertMirror :exec
INSERT INTO audience_schedules (audience_id, workspace_id, next_refresh_at, enabled)
VALUES (sqlc.arg(audience_id)::text, sqlc.arg(workspace_id)::text,
        (sqlc.narg(next_refresh_at)::timestamptz)::timestamp, sqlc.arg(enabled)::boolean)
ON CONFLICT (audience_id) DO UPDATE
SET workspace_id = EXCLUDED.workspace_id, next_refresh_at = EXCLUDED.next_refresh_at,
    enabled = EXCLUDED.enabled, updated_at = now()
WHERE (audience_schedules.workspace_id, audience_schedules.next_refresh_at, audience_schedules.enabled)
      IS DISTINCT FROM (EXCLUDED.workspace_id, EXCLUDED.next_refresh_at, EXCLUDED.enabled);

-- Mirror of a retry time that is already a naive UTC value (job.next_run_at).
-- name: AudSchedUpsertMirrorNaive :exec
INSERT INTO audience_schedules (audience_id, workspace_id, next_refresh_at, enabled)
VALUES (sqlc.arg(audience_id)::text, sqlc.arg(workspace_id)::text, sqlc.narg(next_refresh_at)::timestamp, sqlc.arg(enabled)::boolean)
ON CONFLICT (audience_id) DO UPDATE
SET workspace_id = EXCLUDED.workspace_id, next_refresh_at = EXCLUDED.next_refresh_at,
    enabled = EXCLUDED.enabled, updated_at = now()
WHERE (audience_schedules.workspace_id, audience_schedules.next_refresh_at, audience_schedules.enabled)
      IS DISTINCT FROM (EXCLUDED.workspace_id, EXCLUDED.next_refresh_at, EXCLUDED.enabled);

-- Tenant-scoped delete: a mirror row of another workspace is never removed.
-- name: AudSchedDeleteMirror :exec
DELETE FROM audience_schedules
WHERE audience_id = sqlc.arg(audience_id)::text
  AND workspace_id = coalesce(current_setting('app.workspace_id', true), '');

-- name: AudSchedMarkFailed :exec
UPDATE audiences
SET refresh_health = 'degraded', last_refresh_error = sqlc.arg(last_refresh_error)::text,
    consecutive_refresh_failures = coalesce(consecutive_refresh_failures, 0) + 1, updated_at = now()
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AudSchedJobNextRun :one
SELECT next_run_at FROM jobs WHERE id = sqlc.arg(id)::int;

-- Cross-tenant read of the non-RLS mirror (ids and timestamps only). The naive column
-- is UTC wall time: compare it as timestamptz.
-- name: AudSchedDue :many
SELECT audience_id, workspace_id, next_refresh_at
FROM audience_schedules
WHERE enabled IS TRUE
  AND (next_refresh_at IS NULL OR (next_refresh_at AT TIME ZONE 'UTC') <= sqlc.arg(now)::timestamptz)
ORDER BY audience_id;
