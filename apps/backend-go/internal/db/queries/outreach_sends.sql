-- plan 09 task 13: outreach send ledger (outreach_sends) and stats. Every
-- tenant statement runs inside tenancy.InWorkspace (FORCE RLS) and also filters
-- on workspace_id explicitly. OutreachSendLookupAnyWorkspace is the only
-- non-scoped statement: it calls the SECURITY DEFINER routing-tuple function.

-- name: OutreachSendGetByIdem :one
SELECT id, workspace_id, sequence_id, enrollment_id, lead_id, step_number, to_email,
       subject, status, skip_reason, message_id, idempotency_key, charged_usd, migrated,
       error, sent_at, opened_at, replied_at, bounced_at, created_at, updated_at
FROM outreach_sends
WHERE workspace_id = sqlc.arg(workspace_id)::text
  AND idempotency_key = sqlc.arg(idempotency_key)::text
LIMIT 1;

-- name: OutreachSendInsertInFlight :execrows
-- at-most-once marker: a concurrent insert of the same idempotency key does
-- nothing (rows = 0) instead of aborting the transaction.
INSERT INTO outreach_sends (workspace_id, sequence_id, enrollment_id, lead_id, step_number,
                            to_email, subject, status, idempotency_key, charged_usd)
VALUES (sqlc.arg(workspace_id)::text, sqlc.narg(sequence_id)::text, sqlc.narg(enrollment_id)::int,
        sqlc.narg(lead_id)::int, sqlc.arg(step_number)::int, sqlc.arg(to_email)::text,
        sqlc.arg(subject)::text, 'in_flight', sqlc.arg(idempotency_key)::text, 0.0)
ON CONFLICT ON CONSTRAINT uq_outreach_send_idem DO NOTHING;

-- name: OutreachSendInsertTerminal :execrows
-- Terminal row for a skip/failure that never reached in_flight; an existing
-- idempotency key is left untouched (Python _record_terminal).
INSERT INTO outreach_sends (workspace_id, sequence_id, enrollment_id, lead_id, step_number,
                            to_email, subject, status, skip_reason, error, idempotency_key)
VALUES (sqlc.arg(workspace_id)::text, sqlc.narg(sequence_id)::text, sqlc.narg(enrollment_id)::int,
        sqlc.narg(lead_id)::int, sqlc.arg(step_number)::int, sqlc.arg(to_email)::text,
        sqlc.arg(subject)::text, sqlc.arg(status)::text, sqlc.narg(skip_reason)::text,
        sqlc.arg(error)::text, sqlc.arg(idempotency_key)::text)
ON CONFLICT ON CONSTRAINT uq_outreach_send_idem DO NOTHING;

-- name: OutreachSendFinalize :execrows
-- Only the given fields change (Python _finalize_send): NULL/empty args keep
-- the stored value; status is always applied when non-empty.
UPDATE outreach_sends
SET status = COALESCE(NULLIF(sqlc.arg(status)::text, ''), status),
    skip_reason = COALESCE(NULLIF(sqlc.arg(skip_reason)::text, ''), skip_reason),
    message_id = COALESCE(NULLIF(sqlc.arg(message_id)::text, ''), message_id),
    error = COALESCE(NULLIF(sqlc.arg(error)::text, ''), error),
    sent_at = COALESCE(sqlc.narg(sent_at)::timestamptz, sent_at),
    bounced_at = COALESCE(sqlc.narg(bounced_at)::timestamptz, bounced_at),
    charged_usd = COALESCE(sqlc.narg(charged_usd)::float8, charged_usd),
    updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id)::text
  AND idempotency_key = sqlc.arg(idempotency_key)::text;

-- name: OutreachSendGetByMessageID :one
SELECT id, sequence_id, enrollment_id, lead_id, step_number, to_email, subject, status,
       skip_reason, message_id, charged_usd, migrated, error, sent_at, created_at
FROM outreach_sends
WHERE workspace_id = sqlc.arg(workspace_id)::text AND message_id = sqlc.arg(message_id)::text
ORDER BY id DESC
LIMIT 1;

-- name: OutreachSendLookupAnyWorkspace :one
SELECT f.workspace_id::text AS workspace_id, f.send_id::int AS send_id, COALESCE(f.sequence_id, '')::text AS sequence_id,
       COALESCE(f.enrollment_id, 0)::int AS enrollment_id,
       (f.enrollment_id IS NOT NULL)::bool AS has_enrollment, f.to_email::text AS to_email
FROM outreach_send_by_message_id(sqlc.arg(message_id)::text) AS f(workspace_id, send_id, sequence_id, enrollment_id, to_email);

-- name: OutreachSendRecentLive :one
SELECT id, sequence_id, enrollment_id, lead_id, step_number, to_email, subject, status,
       skip_reason, message_id, charged_usd, migrated, error, sent_at, created_at
FROM outreach_sends
WHERE workspace_id = sqlc.arg(workspace_id)::text AND to_email = sqlc.arg(to_email)::text
  AND status IN ('sent', 'in_flight')
ORDER BY id DESC
LIMIT 1;

-- name: OutreachSendRecentAny :one
SELECT id, sequence_id, enrollment_id, lead_id, step_number, to_email, subject, status,
       skip_reason, message_id, charged_usd, migrated, error, sent_at, created_at
FROM outreach_sends
WHERE workspace_id = sqlc.arg(workspace_id)::text AND to_email = sqlc.arg(to_email)::text
ORDER BY id DESC
LIMIT 1;

-- name: OutreachSendMarkBounced :execrows
UPDATE outreach_sends
SET status = 'bounced',
    bounced_at = COALESCE(bounced_at, now()),
    error = CASE WHEN sqlc.arg(error)::text <> '' THEN left(sqlc.arg(error)::text, 500) ELSE error END,
    updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::int;

-- name: OutreachSendList :many
SELECT id, sequence_id, enrollment_id, lead_id, step_number, to_email, subject, status,
       skip_reason, message_id, charged_usd, migrated, error, sent_at, created_at
FROM outreach_sends
WHERE workspace_id = sqlc.arg(workspace_id)::text
  AND (sqlc.arg(sequence_id)::text = '' OR sequence_id = sqlc.arg(sequence_id)::text)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: OutreachSendCountSentSince :one
SELECT count(*)::int AS n
FROM outreach_sends
WHERE workspace_id = sqlc.arg(workspace_id)::text AND status = 'sent'
  AND sent_at >= sqlc.arg(cutoff)::timestamptz;

-- name: OutreachSendCountSeqSentSince :one
SELECT count(*)::int AS n
FROM outreach_sends
WHERE workspace_id = sqlc.arg(workspace_id)::text AND sequence_id = sqlc.arg(sequence_id)::text
  AND status = 'sent' AND sent_at >= sqlc.arg(cutoff)::timestamptz;

-- name: OutreachSendCountSeqSent :one
SELECT count(*)::int AS n
FROM outreach_sends
WHERE workspace_id = sqlc.arg(workspace_id)::text AND sequence_id = sqlc.arg(sequence_id)::text
  AND status = 'sent';

-- name: OutreachSendEnrollmentStatusCounts :many
SELECT status::text AS status, count(*)::int AS n
FROM outreach_enrollments
WHERE workspace_id = sqlc.arg(workspace_id)::text AND sequence_id = sqlc.arg(sequence_id)::text
GROUP BY status;

-- name: OutreachSendSequenceCounters :one
SELECT COALESCE(bounce_count, 0)::int AS bounce_count,
       COALESCE(complaint_count, 0)::int AS complaint_count,
       COALESCE(auto_paused, false)::bool AS auto_paused
FROM outreach_sequences
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(sequence_id)::text;
