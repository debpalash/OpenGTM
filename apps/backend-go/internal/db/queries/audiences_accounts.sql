-- plan 10 task 17: audience accounts rollup. audience_members and signals are FORCE RLS: run inside
-- tenancy.InWorkspace and keep the explicit workspace predicates.

-- name: AudAccExists :one
SELECT EXISTS (
  SELECT 1 FROM audiences WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
) AS found;

-- name: AudAccMemberCount :one
SELECT count(*)::bigint AS n
FROM audience_members
WHERE workspace_id = sqlc.arg(workspace_id)::text AND audience_id = sqlc.arg(audience_id)::text;

-- Members in id order, one keyset page at a time.
-- name: AudAccMembersPage :many
SELECT id, lead_id, snapshot
FROM audience_members
WHERE workspace_id = sqlc.arg(workspace_id)::text AND audience_id = sqlc.arg(audience_id)::text
  AND id > sqlc.arg(after_id)::int
ORDER BY id ASC
LIMIT sqlc.arg(page_limit)::int;

-- Signal count and weight per member lead.
-- name: AudAccSignals :many
SELECT s.lead_id AS lead_id, count(s.id)::bigint AS n, coalesce(sum(s.weight), 0)::bigint AS weight
FROM signals s
JOIN audience_members m ON m.lead_id = s.lead_id AND m.workspace_id = s.workspace_id
WHERE m.workspace_id = sqlc.arg(workspace_id)::text AND m.audience_id = sqlc.arg(audience_id)::text
GROUP BY s.lead_id;
