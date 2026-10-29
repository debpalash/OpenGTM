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
-- Only the fields flagged set_* change (model_dump(exclude_none=True)).
-- updated_at bumps only when a column really changes: SQLAlchemy skips an UPDATE
-- whose assigned values equal the loaded ones, so the ORM onupdate never fires.
WITH n AS (
  SELECT s.id,
    CASE WHEN sqlc.arg(set_name)::boolean THEN sqlc.arg(name)::text ELSE s.name END AS name,
    CASE WHEN sqlc.arg(set_description)::boolean THEN sqlc.arg(description)::text ELSE s.description END AS description,
    CASE WHEN sqlc.arg(set_steps)::boolean THEN sqlc.arg(steps)::text::json ELSE s.steps END AS steps,
    CASE WHEN sqlc.arg(set_status)::boolean THEN sqlc.arg(status)::text ELSE s.status END AS status,
    CASE WHEN sqlc.arg(set_daily_limit)::boolean THEN sqlc.arg(daily_limit)::int ELSE s.daily_limit END AS daily_limit,
    CASE WHEN sqlc.arg(set_send_window_start)::boolean THEN sqlc.arg(send_window_start)::int ELSE s.send_window_start END AS send_window_start,
    CASE WHEN sqlc.arg(set_send_window_end)::boolean THEN sqlc.arg(send_window_end)::int ELSE s.send_window_end END AS send_window_end,
    CASE WHEN sqlc.arg(set_send_window_tz)::boolean THEN sqlc.arg(send_window_tz)::text ELSE s.send_window_tz END AS send_window_tz,
    CASE WHEN sqlc.arg(set_consent_basis)::boolean THEN sqlc.arg(consent_basis)::text ELSE s.consent_basis END AS consent_basis
  FROM outreach_sequences s
  WHERE s.workspace_id = sqlc.arg(workspace_id)::text AND s.id = sqlc.arg(id)::text
)
UPDATE outreach_sequences t SET
  name = n.name, description = n.description, steps = n.steps, status = n.status,
  daily_limit = n.daily_limit, send_window_start = n.send_window_start,
  send_window_end = n.send_window_end, send_window_tz = n.send_window_tz,
  consent_basis = n.consent_basis,
  updated_at = CASE WHEN (t.name, t.description, t.steps::jsonb, t.status, t.daily_limit, t.send_window_start,
                          t.send_window_end, t.send_window_tz, t.consent_basis)
                       IS DISTINCT FROM
                       (n.name, n.description, n.steps::jsonb, n.status, n.daily_limit, n.send_window_start,
                          n.send_window_end, n.send_window_tz, n.consent_basis)
                    THEN now() ELSE t.updated_at END
FROM n
WHERE t.id = n.id AND t.workspace_id = sqlc.arg(workspace_id)::text
RETURNING t.id, t.workspace_id, t.name, t.description, t.steps::text AS steps, t.status, t.daily_limit,
          t.send_window_start, t.send_window_end, t.send_window_tz, t.consent_basis, t.bounce_count,
          t.complaint_count, t.auto_paused, t.created_at, t.updated_at;

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
