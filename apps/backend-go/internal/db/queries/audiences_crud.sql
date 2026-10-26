-- plan 10 task 15: audience CRUD routes. audiences is FORCE RLS: run inside
-- tenancy.InWorkspace and keep the explicit workspace predicate. SELECT * maps to
-- the gen.Audience model (workspace_id included).

-- name: AudCrudInsert :exec
INSERT INTO audiences (id, workspace_id, name, description, filters, refresh_enabled, refresh_interval_minutes)
VALUES (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(name)::text, sqlc.arg(description)::text,
        sqlc.arg(filters)::json, sqlc.arg(refresh_enabled)::boolean, sqlc.arg(refresh_interval_minutes)::int);

-- id is the tiebreak Python leaves undefined (plan 10 D8).
-- name: AudCrudList :many
SELECT * FROM audiences
WHERE workspace_id = sqlc.arg(workspace_id)::text
ORDER BY updated_at DESC, id ASC
LIMIT sqlc.arg(row_limit)::int;

-- name: AudCrudGet :one
SELECT * FROM audiences
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AudCrudGetForUpdate :one
SELECT * FROM audiences
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
FOR UPDATE;

-- SQLAlchemy flushes only changed attributes and bumps updated_at (onupdate=now()); the caller
-- runs this only when something changed.
-- name: AudCrudUpdate :exec
UPDATE audiences
SET name = sqlc.arg(name)::text, description = sqlc.arg(description)::text, filters = sqlc.arg(filters)::json,
    refresh_enabled = sqlc.arg(refresh_enabled)::boolean, refresh_interval_minutes = sqlc.arg(refresh_interval_minutes)::int,
    updated_at = now()
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AudCrudDelete :execrows
DELETE FROM audiences
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;
