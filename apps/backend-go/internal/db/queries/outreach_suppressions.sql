-- plan 09 (outreach): suppression list and the per-sequence circuit breaker.
-- Tenant statements run inside tenancy.InWorkspace and filter workspace_id too.

-- name: OutreachSuppressionExists :one
SELECT EXISTS (SELECT 1 FROM outreach_suppressions
               WHERE workspace_id = sqlc.arg(workspace_id)::text
                 AND email = ANY(sqlc.arg(emails)::text[]))::boolean;

-- name: OutreachSuppressionAdd :one
-- ON CONFLICT DO NOTHING: the insert never rewrites an existing row (OutreachSuppressionLock upgrades unlocked ones).
WITH ins AS (
  INSERT INTO outreach_suppressions (workspace_id, email, reason, source, locked)
  VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(email)::text, sqlc.arg(reason)::text,
          sqlc.arg(source)::text, sqlc.arg(locked)::boolean)
  ON CONFLICT (workspace_id, email) DO NOTHING
  RETURNING 1
)
SELECT EXISTS (SELECT 1 FROM ins)::boolean AS added;

-- name: OutreachSuppressionLock :execrows
-- Explicit opt-out (unsubscribe/complaint): upgrade existing unlocked rows to locked; locked rows keep their reason.
UPDATE outreach_suppressions
SET locked = true, reason = sqlc.arg(reason)::text, source = sqlc.arg(source)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text
  AND email = ANY(sqlc.arg(emails)::text[])
  AND locked IS NOT TRUE;

-- name: OutreachSuppressionList :many
SELECT id, email, reason, source, locked, created_at
FROM outreach_suppressions
WHERE workspace_id = sqlc.arg(workspace_id)::text
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: OutreachSuppressionGet :one
SELECT id, locked FROM outreach_suppressions
WHERE workspace_id = sqlc.arg(workspace_id)::text AND email = sqlc.arg(email)::text;

-- name: OutreachSuppressionDelete :execrows
DELETE FROM outreach_suppressions
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::int;

-- name: OutreachSeqBumpCounter :one
-- Python record_bounce_and_maybe_pause: the counter moves first, even when the
-- breaker will not evaluate. No row (unknown or foreign id) returns no rows.
UPDATE outreach_sequences SET
  bounce_count = CASE WHEN sqlc.arg(complaint)::boolean THEN bounce_count ELSE COALESCE(bounce_count, 0) + 1 END,
  complaint_count = CASE WHEN sqlc.arg(complaint)::boolean THEN COALESCE(complaint_count, 0) + 1 ELSE complaint_count END,
  updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text
RETURNING COALESCE(bounce_count, 0)::int AS bounce_count,
          COALESCE(complaint_count, 0)::int AS complaint_count,
          COALESCE(auto_paused, false)::boolean AS auto_paused;

-- name: OutreachSendCountSentOrBounced :one
SELECT count(*)::int FROM outreach_sends
WHERE workspace_id = sqlc.arg(workspace_id)::text AND sequence_id = sqlc.arg(sequence_id)::text
  AND status IN ('sent', 'bounced');

-- name: OutreachSeqAutoPause :exec
UPDATE outreach_sequences SET auto_paused = true, status = 'paused', updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text;
