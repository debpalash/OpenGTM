-- plan 10 task 20: inbound token and receipt routes (routers/destinations.py rotate/status/revoke/receipts).

-- The destination row lock serializes concurrent rotate/revoke of one destination, so two rotates cannot both
-- insert an active token (Python lets them; documented hardening).
-- name: DestTokDestLock :one
SELECT destination_type FROM audience_destinations
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text
FOR UPDATE;

-- name: DestTokDestType :one
SELECT destination_type FROM audience_destinations
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text;

-- created_at DESC, id DESC; the cursor is (created_at, id) of the last row of the previous page. limit is page+1.
-- name: DestTokReceiptsPage :many
SELECT id, destination_id, provider, external_event_id, external_record_id, lead_id, status,
       conflict_policy, applied_fields, ignored_fields, error, created_at
FROM destination_inbound_receipts
WHERE workspace_id = sqlc.arg(workspace_id)::text AND destination_id = sqlc.arg(destination_id)::text
  AND (NOT sqlc.arg(has_cursor)::boolean
       OR created_at < sqlc.arg(cursor_at)::timestamptz
       OR (created_at = sqlc.arg(cursor_at)::timestamptz AND id < sqlc.arg(cursor_id)::text))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;
