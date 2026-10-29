-- plan 10 task 16: audience members / events reads. Both tables are FORCE RLS: run inside
-- tenancy.InWorkspace and keep the explicit workspace predicate.

-- name: AudReadExists :one
SELECT EXISTS (
  SELECT 1 FROM audiences WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
) AS found;

-- name: AudReadMembers :many
SELECT id, lead_id, snapshot, joined_at, last_seen_at
FROM audience_members
WHERE workspace_id = sqlc.arg(workspace_id)::text AND audience_id = sqlc.arg(audience_id)::text
  AND (sqlc.narg(before_id)::bigint IS NULL OR id < sqlc.narg(before_id)::bigint)
ORDER BY id DESC
OFFSET sqlc.arg(row_offset)::int
LIMIT sqlc.arg(row_limit)::int;

-- name: AudReadEvents :many
SELECT id, audience_id, lead_id, event_type, snapshot, created_at
FROM audience_membership_events
WHERE workspace_id = sqlc.arg(workspace_id)::text AND audience_id = sqlc.arg(audience_id)::text
  AND (sqlc.narg(before_id)::bigint IS NULL OR id < sqlc.narg(before_id)::bigint)
ORDER BY id DESC
LIMIT sqlc.arg(row_limit)::int;
