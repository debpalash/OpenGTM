-- plan 10 task 12: append-only membership events (audience_membership_events is FORCE RLS).

-- name: AudRefreshEventInsert :exec
INSERT INTO audience_membership_events (workspace_id, audience_id, lead_id, event_type, snapshot, refresh_id)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(audience_id)::text, sqlc.arg(lead_id)::int,
        sqlc.arg(event_type)::text, sqlc.arg(snapshot)::json, sqlc.arg(refresh_id)::text);

-- One refresh's events in stable keyset pages.
-- name: AudRefreshEventsPage :many
SELECT id, company_entity_id,person_entity_id, lead_id, event_type
FROM audience_membership_events
WHERE workspace_id = sqlc.arg(workspace_id)::text AND audience_id = sqlc.arg(audience_id)::text
  AND refresh_id = sqlc.arg(refresh_id)::text AND id > sqlc.arg(after_id)::int
ORDER BY id ASC
LIMIT sqlc.arg(page_limit)::int;
