-- Governance audit event listing and CSV keyset batches (tenant table under FORCE RLS:
-- run inside tenancy.InWorkspace). created_at is a naive UTC timestamp. Naive since/before
-- compare as timestamps; aware ones (*_tz) and decoded cursors (cursor_tz) are converted
-- to UTC wall time first, so the result does not depend on the session TimeZone.

-- name: ListGovernanceAuditEvents :many
SELECT id, actor_user_id, actor_role, method, route, resource_path,
       response_status, outcome, request_id, metadata_json, created_at
FROM governance_audit_events
WHERE workspace_id = sqlc.arg(workspace_id)::text
  AND (sqlc.narg(actor_user_id)::int IS NULL OR actor_user_id = sqlc.narg(actor_user_id)::int)
  AND (sqlc.arg(method)::text = '' OR method = sqlc.arg(method)::text)
  AND (sqlc.arg(outcome)::text = '' OR outcome = sqlc.arg(outcome)::text)
  AND (sqlc.narg(since)::timestamp IS NULL OR created_at >= sqlc.narg(since)::timestamp)
  AND (sqlc.narg(before)::timestamp IS NULL OR created_at < sqlc.narg(before)::timestamp)
  AND (sqlc.narg(since_tz)::timestamptz IS NULL OR created_at >= (sqlc.narg(since_tz)::timestamptz AT TIME ZONE 'UTC'))
  AND (sqlc.narg(before_tz)::timestamptz IS NULL OR created_at < (sqlc.narg(before_tz)::timestamptz AT TIME ZONE 'UTC'))
  AND (sqlc.narg(cursor_created_at)::timestamp IS NULL
       OR created_at < sqlc.narg(cursor_created_at)::timestamp
       OR (created_at = sqlc.narg(cursor_created_at)::timestamp AND id < sqlc.narg(cursor_id)::text))
  AND (sqlc.narg(cursor_tz)::timestamptz IS NULL
       OR created_at < (sqlc.narg(cursor_tz)::timestamptz AT TIME ZONE 'UTC')
       OR (created_at = (sqlc.narg(cursor_tz)::timestamptz AT TIME ZONE 'UTC') AND id < sqlc.narg(cursor_id)::text))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;
