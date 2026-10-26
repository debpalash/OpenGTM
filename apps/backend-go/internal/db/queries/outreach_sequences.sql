-- plan 09 (outreach): sequences. Every statement runs inside tenancy.InWorkspace
-- and still filters workspace_id (belt and suspenders with FORCE RLS).

-- name: OutreachSeqCreate :one
INSERT INTO outreach_sequences (id, workspace_id, name, description, steps, status, daily_limit,
                                send_window_start, send_window_end, send_window_tz, consent_basis)
VALUES (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(name)::text, sqlc.arg(description)::text,
        sqlc.arg(steps)::text::json, 'draft', sqlc.arg(daily_limit)::int, sqlc.arg(send_window_start)::int,
        sqlc.arg(send_window_end)::int, sqlc.arg(send_window_tz)::text, sqlc.arg(consent_basis)::text)
RETURNING id, workspace_id, name, description, steps::text AS steps, status, daily_limit, send_window_start,
          send_window_end, send_window_tz, consent_basis, bounce_count, complaint_count, auto_paused,
          created_at, updated_at;

-- name: OutreachSeqList :many
SELECT id, workspace_id, name, description, steps::text AS steps, status, daily_limit, send_window_start,
       send_window_end, send_window_tz, consent_basis, bounce_count, complaint_count, auto_paused,
       created_at, updated_at
FROM outreach_sequences
WHERE workspace_id = sqlc.arg(workspace_id)::text
ORDER BY created_at DESC;

-- name: OutreachSeqGet :one
SELECT id, workspace_id, name, description, steps::text AS steps, status, daily_limit, send_window_start,
       send_window_end, send_window_tz, consent_basis, bounce_count, complaint_count, auto_paused,
       created_at, updated_at
FROM outreach_sequences
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text;

-- name: OutreachSeqExists :one
SELECT EXISTS (SELECT 1 FROM outreach_sequences
               WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text)::boolean;

-- name: OutreachSeqUpdate :one
-- Only the fields flagged set_* change (model_dump(exclude_none=True)); updated_at
-- bumps like the ORM onupdate.
UPDATE outreach_sequences SET
  name = CASE WHEN sqlc.arg(set_name)::boolean THEN sqlc.arg(name)::text ELSE name END,
  description = CASE WHEN sqlc.arg(set_description)::boolean THEN sqlc.arg(description)::text ELSE description END,
  steps = CASE WHEN sqlc.arg(set_steps)::boolean THEN sqlc.arg(steps)::text::json ELSE steps END,
  status = CASE WHEN sqlc.arg(set_status)::boolean THEN sqlc.arg(status)::text ELSE status END,
  daily_limit = CASE WHEN sqlc.arg(set_daily_limit)::boolean THEN sqlc.arg(daily_limit)::int ELSE daily_limit END,
  send_window_start = CASE WHEN sqlc.arg(set_send_window_start)::boolean THEN sqlc.arg(send_window_start)::int ELSE send_window_start END,
  send_window_end = CASE WHEN sqlc.arg(set_send_window_end)::boolean THEN sqlc.arg(send_window_end)::int ELSE send_window_end END,
  send_window_tz = CASE WHEN sqlc.arg(set_send_window_tz)::boolean THEN sqlc.arg(send_window_tz)::text ELSE send_window_tz END,
  consent_basis = CASE WHEN sqlc.arg(set_consent_basis)::boolean THEN sqlc.arg(consent_basis)::text ELSE consent_basis END,
  updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text
RETURNING id, workspace_id, name, description, steps::text AS steps, status, daily_limit, send_window_start,
          send_window_end, send_window_tz, consent_basis, bounce_count, complaint_count, auto_paused,
          created_at, updated_at;

-- name: OutreachSeqDeleteSends :exec
DELETE FROM outreach_sends
WHERE workspace_id = sqlc.arg(workspace_id)::text AND sequence_id = sqlc.arg(sequence_id)::text;

-- name: OutreachSeqDeleteEnrollments :exec
DELETE FROM outreach_enrollments
WHERE workspace_id = sqlc.arg(workspace_id)::text AND sequence_id = sqlc.arg(sequence_id)::text;

-- name: OutreachSeqDelete :execrows
DELETE FROM outreach_sequences
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text;

-- name: OutreachSeqDeleteSchedule :exec
DELETE FROM outreach_schedules
WHERE workspace_id = sqlc.arg(workspace_id)::text AND sequence_id = sqlc.arg(sequence_id)::text;
