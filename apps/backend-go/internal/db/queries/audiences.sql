-- plan 10 task 12: audience refresh (refresh_audience / refresh_audience_observed).
-- audiences is FORCE RLS: run inside tenancy.InWorkspace and keep the explicit
-- workspace predicate. Naive timestamp columns take an aware instant cast through
-- timestamptz, exactly as the audiencesched mirror does (session TimeZone is UTC).

-- name: AudRefreshLock :one
SELECT id, workspace_id, name, filters
FROM audiences
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
FOR UPDATE;

-- SQLAlchemy flushes the changed member_count/refreshed_at and bumps updated_at.
-- name: AudRefreshFinish :exec
UPDATE audiences
SET member_count = sqlc.arg(member_count)::int,
    refreshed_at = (sqlc.arg(refreshed_at)::timestamptz)::timestamp,
    updated_at = now()
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AudRefreshGet :one
SELECT id, name, description, filters, member_count, created_at, updated_at, refreshed_at,
       refresh_enabled, refresh_interval_minutes, next_refresh_at, refresh_health,
       last_refresh_error, consecutive_refresh_failures
FROM audiences
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- Success path of refresh_audience_observed. SQLAlchemy writes (and bumps
-- updated_at) only when an attribute actually changed.
-- name: AudRefreshMarkHealthy :exec
UPDATE audiences
SET refresh_health = 'healthy', last_refresh_error = NULL, consecutive_refresh_failures = 0, updated_at = now()
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
  AND (refresh_health, last_refresh_error, consecutive_refresh_failures) IS DISTINCT FROM ('healthy'::varchar, NULL::text, 0);

-- Failure path: failures + 1 (the lock is not needed: Python reloads and increments).
-- name: AudRefreshMarkDegraded :one
UPDATE audiences
SET refresh_health = 'degraded', last_refresh_error = sqlc.arg(last_error)::text,
    consecutive_refresh_failures = consecutive_refresh_failures + 1, updated_at = now()
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
RETURNING id;
