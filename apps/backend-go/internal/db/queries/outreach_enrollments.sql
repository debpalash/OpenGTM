-- plan 09 (outreach): enrollments. Run inside tenancy.InWorkspace; statements
-- without a workspace argument filter on the transaction's app.workspace_id GUC
-- (belt) on top of FORCE RLS (suspenders).

-- name: OutreachEnrollInsert :one
-- ON CONFLICT DO NOTHING (uq_enroll_ws_seq_lead) keeps the caller's transaction
-- usable; no row returned means already enrolled.
INSERT INTO outreach_enrollments (workspace_id, sequence_id, lead_id, to_email_snapshot, consent_source,
                                  consent_at, current_step, status, next_send_at, sent_count, soft_bounce_count)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(sequence_id)::text, sqlc.arg(lead_id)::int,
        sqlc.arg(to_email_snapshot)::text, sqlc.arg(consent_source)::text, sqlc.narg(consent_at)::timestamptz,
        0, 'pending', now(), 0, 0)
ON CONFLICT ON CONSTRAINT uq_enroll_ws_seq_lead DO NOTHING
RETURNING id;

-- name: OutreachEnrollmentGet :one
SELECT id, workspace_id, sequence_id, lead_id, to_email_snapshot, consent_source, consent_at, current_step,
       status, next_send_at, sent_count, soft_bounce_count, last_sent_at, error
FROM outreach_enrollments
WHERE workspace_id = current_setting('app.workspace_id', true) AND id = sqlc.arg(id)::int;

-- name: OutreachEnrollmentsDue :many
-- Due step: pending/scheduled, next_send_at <= now, no send row yet for the
-- current step; ix_enroll_ws_due order (next_send_at asc, id as tiebreak).
SELECT e.id, e.workspace_id, e.sequence_id, e.lead_id, e.to_email_snapshot, e.consent_source, e.consent_at,
       e.current_step, e.status, e.next_send_at, e.sent_count, e.soft_bounce_count, e.last_sent_at, e.error
FROM outreach_enrollments e
WHERE e.workspace_id = current_setting('app.workspace_id', true)
  AND e.sequence_id = sqlc.arg(sequence_id)::text
  AND e.status IN ('pending', 'scheduled')
  AND e.next_send_at <= now()
  AND NOT EXISTS (SELECT 1 FROM outreach_sends s
                  WHERE s.workspace_id = current_setting('app.workspace_id', true)
                    AND s.sequence_id = sqlc.arg(sequence_id)::text
                    AND s.enrollment_id = e.id
                    AND s.step_number = e.current_step)
ORDER BY e.next_send_at ASC, e.id ASC
LIMIT sqlc.arg(row_limit)::int;

-- name: OutreachEnrollmentReschedule :exec
UPDATE outreach_enrollments
SET next_send_at = sqlc.arg(next_send_at)::timestamptz,
    status = CASE WHEN status = 'pending' THEN 'scheduled' ELSE status END
WHERE workspace_id = current_setting('app.workspace_id', true) AND id = sqlc.arg(id)::int;

-- name: OutreachEnrollmentAdvance :exec
UPDATE outreach_enrollments SET
  status = sqlc.arg(status)::text,
  next_send_at = CASE WHEN sqlc.arg(set_next_send_at)::boolean THEN sqlc.arg(next_send_at)::timestamptz ELSE next_send_at END,
  current_step = CASE WHEN sqlc.arg(set_current_step)::boolean THEN sqlc.arg(current_step)::int ELSE current_step END,
  sent_count = CASE WHEN sqlc.arg(increment_sent)::boolean THEN COALESCE(sent_count, 0) + 1 ELSE sent_count END,
  last_sent_at = CASE WHEN sqlc.arg(increment_sent)::boolean THEN now() ELSE last_sent_at END,
  error = CASE WHEN sqlc.arg(error)::text <> '' THEN sqlc.arg(error)::text ELSE error END
WHERE workspace_id = current_setting('app.workspace_id', true) AND id = sqlc.arg(id)::int;

-- name: OutreachEnrollmentIncSoftBounce :one
UPDATE outreach_enrollments SET soft_bounce_count = COALESCE(soft_bounce_count, 0) + 1
WHERE workspace_id = current_setting('app.workspace_id', true) AND id = sqlc.arg(id)::int
RETURNING soft_bounce_count;
