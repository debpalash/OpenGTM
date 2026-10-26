-- plan 10 (destinations): audience_destinations CRUD. audience_destinations is RLS
-- (callers run inside tenancy.InWorkspace); every statement also filters on the
-- explicit workspace id. destination_inbound_tokens is a non-RLS auth-plane table.
-- Timestamps are naive UTC columns.

-- name: DestInsert :one
INSERT INTO audience_destinations (id, workspace_id, audience_id, name, destination_type, enabled, config, field_map)
VALUES (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(audience_id)::text, sqlc.arg(name)::text,
        sqlc.arg(destination_type)::text, sqlc.arg(enabled)::boolean, sqlc.arg(config)::json, sqlc.arg(field_map)::json)
RETURNING *;

-- name: DestGet :one
SELECT * FROM audience_destinations
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text;

-- updated_at = now() mirrors SQLAlchemy's onupdate=func.now().
-- name: DestUpdate :one
UPDATE audience_destinations SET
  name = COALESCE(sqlc.narg(name)::text, name),
  enabled = COALESCE(sqlc.narg(enabled)::boolean, enabled),
  config = COALESCE(sqlc.narg(config)::json, config),
  field_map = COALESCE(sqlc.narg(field_map)::json, field_map),
  updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text
RETURNING *;

-- Latest run = rank 1 by (created_at DESC, id DESC); destinations newest-updated first.
-- name: DestListWithLatestRun :many
WITH ranked AS (
  SELECT r.id AS run_id, r.destination_id,
         row_number() OVER (PARTITION BY r.destination_id ORDER BY r.created_at DESC, r.id DESC) AS rnk
  FROM destination_runs r
  WHERE r.workspace_id = sqlc.arg(workspace_id)::text
)
SELECT d.id, d.workspace_id, d.audience_id, d.name, d.destination_type, d.enabled, d.config, d.field_map,
       d.health_status, d.last_error, d.last_success_at, d.created_at, d.updated_at,
       lr.id AS run_id, lr.workspace_id AS run_workspace_id, lr.destination_id AS run_destination_id,
       lr.status AS run_status, lr.requested_by AS run_requested_by,
       lr.attempted AS run_attempted, lr.succeeded AS run_succeeded, lr.failed AS run_failed,
       lr.skipped AS run_skipped, lr.error AS run_error, lr.started_at AS run_started_at,
       lr.finished_at AS run_finished_at, lr.created_at AS run_created_at
FROM audience_destinations d
LEFT JOIN ranked rk ON rk.destination_id = d.id AND rk.rnk = 1
LEFT JOIN destination_runs lr ON lr.id = rk.run_id AND lr.workspace_id = d.workspace_id
WHERE d.workspace_id = sqlc.arg(workspace_id)::text
  AND (sqlc.arg(audience_id)::text = '' OR d.audience_id = sqlc.arg(audience_id)::text)
ORDER BY d.updated_at DESC, d.id DESC;

-- name: DestEnabledForAudience :many
SELECT id, destination_type FROM audience_destinations
WHERE workspace_id = sqlc.arg(workspace_id)::text AND audience_id = sqlc.arg(audience_id)::text AND enabled
ORDER BY created_at, id;

-- name: DestRevokeInboundTokens :execrows
UPDATE destination_inbound_tokens SET revoked_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND destination_id = sqlc.arg(destination_id)::text
  AND revoked_at IS NULL;

-- name: DestDelete :execrows
DELETE FROM audience_destinations
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text;

-- name: DestMarkDegraded :exec
UPDATE audience_destinations SET health_status = 'degraded', last_error = sqlc.arg(last_error)::text, updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text;

-- name: DestSetLastSuccess :exec
UPDATE audience_destinations SET last_success_at = sqlc.arg(last_success_at)::timestamp, updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text;
