-- plan 10 task 12: materialized membership (audience_members is FORCE RLS).

-- name: AudRefreshMembersByLeads :many
SELECT id, lead_id, snapshot
FROM audience_members
WHERE workspace_id = sqlc.arg(workspace_id)::text AND audience_id = sqlc.arg(audience_id)::text
  AND lead_id = ANY(sqlc.arg(lead_ids)::int[]);

-- name: AudRefreshMemberInsert :exec
INSERT INTO audience_members (workspace_id, audience_id, lead_id, snapshot, refresh_token, joined_at, last_seen_at)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(audience_id)::text, sqlc.arg(lead_id)::int, sqlc.arg(snapshot)::json,
        sqlc.arg(refresh_token)::text, (sqlc.arg(now)::timestamptz AT TIME ZONE 'UTC'), (sqlc.arg(now)::timestamptz AT TIME ZONE 'UTC'));

-- name: AudRefreshMemberTouch :exec
UPDATE audience_members
SET snapshot = sqlc.arg(snapshot)::json, refresh_token = sqlc.arg(refresh_token)::text,
    last_seen_at = (sqlc.arg(now)::timestamptz AT TIME ZONE 'UTC')
WHERE id = sqlc.arg(id)::int AND workspace_id = sqlc.arg(workspace_id)::text AND audience_id = sqlc.arg(audience_id)::text;

-- Members the refresh did not see (NULL or another token), keyset by id.
-- name: AudRefreshUnseenMembers :many
SELECT id, lead_id, snapshot
FROM audience_members
WHERE workspace_id = sqlc.arg(workspace_id)::text AND audience_id = sqlc.arg(audience_id)::text
  AND (refresh_token IS NULL OR refresh_token <> sqlc.arg(refresh_token)::text)
  AND id > sqlc.arg(after_id)::int
ORDER BY id ASC
LIMIT sqlc.arg(page_limit)::int;

-- name: AudRefreshMembersDelete :exec
DELETE FROM audience_members
WHERE workspace_id = sqlc.arg(workspace_id)::text AND audience_id = sqlc.arg(audience_id)::text
  AND id = ANY(sqlc.arg(ids)::int[]);
