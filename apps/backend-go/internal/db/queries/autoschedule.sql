-- plan 08 (automations): schedule enqueue and the scheduled_triggers mirror.
-- Trigger statements run inside tenancy.InWorkspace; the mirror and jobs are
-- non-RLS control tables.

-- name: AutoScheduleTrigger :one
SELECT id, workspace_id, enabled, trigger_type, trigger_config::text AS trigger_config, schedule_anchor
FROM triggers
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: InsertScheduleJob :one
-- Same row as job_scheduling.enqueue_job_once for a trigger_eval, with the
-- Python next_run_at (the scheduled instant, naive UTC).
INSERT INTO jobs (executor, type, payload, workspace_id, priority, fire_key, status,
                  created_at, next_run_at, max_retries, retry_count)
VALUES (sqlc.arg(executor)::text, 'trigger_eval', sqlc.arg(payload)::json,
        NULLIF(sqlc.arg(workspace_id)::text, ''), 1, sqlc.arg(fire_key)::text, 'pending',
        (now() AT TIME ZONE 'UTC'), sqlc.arg(next_run_at)::timestamp, 3, 0)
ON CONFLICT (fire_key) WHERE status IN ('pending', 'processing') DO NOTHING
RETURNING id;

-- name: SetTriggerNextRun :exec
UPDATE triggers SET next_run_at = sqlc.arg(next_run_at)::timestamptz, updated_at = now()
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: UpsertScheduledTrigger :exec
INSERT INTO scheduled_triggers (trigger_id, workspace_id, next_run_at, enabled, updated_at)
VALUES (sqlc.arg(trigger_id)::text, sqlc.arg(workspace_id)::text, sqlc.narg(next_run_at)::timestamptz,
        sqlc.arg(enabled)::boolean, now())
ON CONFLICT (trigger_id) DO UPDATE
SET workspace_id = EXCLUDED.workspace_id, next_run_at = EXCLUDED.next_run_at,
    enabled = EXCLUDED.enabled, updated_at = now();

-- name: DeleteScheduledTrigger :exec
DELETE FROM scheduled_triggers WHERE trigger_id = sqlc.arg(trigger_id)::text;

-- name: DueScheduledTriggers :many
-- Cross-tenant read of the non-RLS mirror (ids and timestamps only).
SELECT trigger_id, workspace_id FROM scheduled_triggers
WHERE enabled IS TRUE AND (next_run_at IS NULL OR next_run_at <= sqlc.arg(now)::timestamptz)
ORDER BY trigger_id;
