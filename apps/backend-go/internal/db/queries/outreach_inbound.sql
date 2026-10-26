-- plan 09 (outreach): inbound dedup ledger (RLS) and the inbound schedule mirror (non-RLS).

-- name: OutreachInboundRecord :one
-- Both unique constraints (ws,uidvalidity,uid) and (ws,source_message_id) collapse to
-- DO NOTHING, so a replay never aborts the caller's transaction.
WITH ins AS (
  INSERT INTO outreach_inbound_messages (workspace_id, imap_uid, uidvalidity, source_message_id,
                                         matched_send_id, kind, recipient, diagnostic, processed_at)
  VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(imap_uid)::text, sqlc.arg(uidvalidity)::text,
          sqlc.arg(source_message_id)::text, sqlc.narg(matched_send_id)::int, sqlc.arg(kind)::text,
          sqlc.arg(recipient)::text, sqlc.arg(diagnostic)::text, now())
  ON CONFLICT DO NOTHING
  RETURNING 1
)
SELECT EXISTS (SELECT 1 FROM ins)::boolean AS created;

-- name: OutreachInboundCount :one
SELECT count(*)::int FROM outreach_inbound_messages WHERE workspace_id = sqlc.arg(workspace_id)::text;

-- name: OutreachInboundScheduleGet :one
SELECT workspace_id, next_poll_at, enabled, consecutive_failures, uidvalidity, last_uid, updated_at
FROM outreach_inbound_schedules WHERE workspace_id = sqlc.arg(workspace_id)::text;

-- name: OutreachInboundScheduleUpsert :exec
-- next_poll_at and enabled always overwrite; the other columns only when flagged set_*.
INSERT INTO outreach_inbound_schedules (workspace_id, next_poll_at, enabled, consecutive_failures,
                                        uidvalidity, last_uid, updated_at)
VALUES (sqlc.arg(workspace_id)::text, sqlc.narg(next_poll_at)::timestamptz, sqlc.arg(enabled)::boolean,
        CASE WHEN sqlc.arg(set_failures)::boolean THEN sqlc.arg(consecutive_failures)::int ELSE 0 END,
        CASE WHEN sqlc.arg(set_uidvalidity)::boolean THEN sqlc.arg(uidvalidity)::text END,
        CASE WHEN sqlc.arg(set_last_uid)::boolean THEN sqlc.arg(last_uid)::text END, now())
ON CONFLICT (workspace_id) DO UPDATE SET
  next_poll_at = EXCLUDED.next_poll_at,
  enabled = EXCLUDED.enabled,
  consecutive_failures = CASE WHEN sqlc.arg(set_failures)::boolean THEN EXCLUDED.consecutive_failures
                              ELSE outreach_inbound_schedules.consecutive_failures END,
  uidvalidity = CASE WHEN sqlc.arg(set_uidvalidity)::boolean THEN EXCLUDED.uidvalidity
                     ELSE outreach_inbound_schedules.uidvalidity END,
  last_uid = CASE WHEN sqlc.arg(set_last_uid)::boolean THEN EXCLUDED.last_uid
                  ELSE outreach_inbound_schedules.last_uid END,
  updated_at = now();

-- name: OutreachInboundScheduleDisable :exec
UPDATE outreach_inbound_schedules SET enabled = false, next_poll_at = NULL, updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id)::text;

-- name: OutreachInboundRecipientsForSend :many
-- Recipients already recorded for one send and kind (breaker de-dup, plan 09 final M1). Bounded.
SELECT COALESCE(recipient, '')::text AS recipient
FROM outreach_inbound_messages
WHERE workspace_id = sqlc.arg(workspace_id)::text AND matched_send_id = sqlc.arg(matched_send_id)::int
  AND kind = sqlc.arg(kind)::text
ORDER BY id LIMIT 500;
