-- plan 09 (outreach): the non-RLS outreach_schedules mirror (read without a
-- workspace GUC at ticker cold start).

-- name: OutreachScheduleUpsert :exec
INSERT INTO outreach_schedules (sequence_id, workspace_id, next_tick_at, enabled, updated_at)
VALUES (sqlc.arg(sequence_id)::text, sqlc.arg(workspace_id)::text, sqlc.narg(next_tick_at)::timestamptz,
        sqlc.arg(enabled)::boolean, now())
ON CONFLICT (sequence_id) DO UPDATE
SET workspace_id = EXCLUDED.workspace_id, next_tick_at = EXCLUDED.next_tick_at,
    enabled = EXCLUDED.enabled, updated_at = now()
WHERE outreach_schedules.workspace_id = EXCLUDED.workspace_id;

-- name: OutreachScheduleDisable :exec
UPDATE outreach_schedules SET enabled = false, updated_at = now()
WHERE sequence_id = sqlc.arg(sequence_id)::text
  AND workspace_id = current_setting('app.workspace_id', true);

-- name: OutreachScheduleGet :one
SELECT sequence_id, workspace_id, next_tick_at, enabled, updated_at
FROM outreach_schedules WHERE sequence_id = sqlc.arg(sequence_id)::text;
