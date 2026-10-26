-- Research playbooks (internal/playbooks). Run inside tenancy.InWorkspace:
-- research_playbooks, playbook_runs and playbook_results are FORCE RLS;
-- playbook_schedules and jobs are global tables, so every statement filters
-- explicitly. Timestamps are naive UTC columns.

-- name: PbInsertPlaybook :one
INSERT INTO research_playbooks (id, workspace_id, name, description, prompt_template, steps, output_format,
                                max_steps, cell_budget_usd, version, enabled, created_at, updated_at)
VALUES (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(name)::text, sqlc.arg(description)::text,
        sqlc.arg(prompt_template)::text, sqlc.arg(steps)::json, sqlc.arg(output_format)::text,
        sqlc.arg(max_steps)::int, sqlc.arg(cell_budget_usd)::float8, 1, true,
        (now() AT TIME ZONE 'UTC'), (now() AT TIME ZONE 'UTC'))
ON CONFLICT (workspace_id, name) DO NOTHING
RETURNING *;

-- name: PbGetPlaybook :one
SELECT * FROM research_playbooks
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text;

-- name: PbLockPlaybook :one
SELECT * FROM research_playbooks
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text
FOR UPDATE;

-- name: PbListPlaybooks :many
SELECT * FROM research_playbooks
WHERE workspace_id = sqlc.arg(workspace_id)::text
ORDER BY updated_at DESC, id
LIMIT sqlc.arg(row_limit)::int;

-- name: PbNameTaken :one
SELECT EXISTS (
  SELECT 1 FROM research_playbooks
  WHERE workspace_id = sqlc.arg(workspace_id)::text AND name = sqlc.arg(name)::text AND id <> sqlc.arg(id)::text
) AS taken;

-- name: PbAudienceExists :one
SELECT EXISTS (
  SELECT 1 FROM audiences WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text
) AS found;

-- name: PbUpdatePlaybook :one
UPDATE research_playbooks SET
  name = sqlc.arg(name)::text,
  description = sqlc.arg(description)::text,
  prompt_template = sqlc.arg(prompt_template)::text,
  steps = sqlc.arg(steps)::json,
  output_format = sqlc.arg(output_format)::text,
  max_steps = sqlc.arg(max_steps)::int,
  cell_budget_usd = sqlc.arg(cell_budget_usd)::float8,
  version = sqlc.arg(version)::int,
  enabled = sqlc.arg(enabled)::bool,
  schedule_audience_id = sqlc.narg(schedule_audience_id)::text,
  schedule_interval_minutes = sqlc.narg(schedule_interval_minutes)::int,
  next_run_at = sqlc.narg(next_run_at)::timestamp,
  updated_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text
RETURNING *;

-- name: PbDeletePlaybook :execrows
DELETE FROM research_playbooks
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text;

-- name: PbActiveRunExists :one
SELECT EXISTS (
  SELECT 1 FROM playbook_runs
  WHERE workspace_id = sqlc.arg(workspace_id)::text AND playbook_id = sqlc.arg(playbook_id)::text
    AND status IN ('pending', 'running')
) AS active;

-- Identifier-only schedule mirror (no RLS): written with its playbook.
-- name: PbUpsertScheduleMirror :exec
INSERT INTO playbook_schedules (playbook_id, workspace_id, enabled, next_run_at, updated_at)
VALUES (sqlc.arg(playbook_id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(enabled)::bool,
        sqlc.narg(next_run_at)::timestamp, (now() AT TIME ZONE 'UTC'))
ON CONFLICT (playbook_id) DO UPDATE
SET workspace_id = EXCLUDED.workspace_id, enabled = EXCLUDED.enabled,
    next_run_at = EXCLUDED.next_run_at, updated_at = EXCLUDED.updated_at;

-- name: PbDeleteScheduleMirror :exec
DELETE FROM playbook_schedules
WHERE playbook_id = sqlc.arg(playbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: PbCancelScheduleJobs :exec
UPDATE jobs SET status = 'cancelled'
WHERE type = 'research_playbook_schedule' AND status = 'pending'
  AND workspace_id = sqlc.arg(workspace_id)::text
  AND starts_with(fire_key, sqlc.arg(fire_prefix)::text);

-- Single-flight scheduled-occurrence job (job_scheduling.enqueue_job_once with
-- next_run_at); lock and active check use LockJobFireKey / ActiveJobByFireKey.
-- name: PbInsertScheduleJob :one
INSERT INTO jobs (executor, type, payload, workspace_id, priority, fire_key, status,
                  created_at, next_run_at, max_retries, retry_count)
VALUES (sqlc.arg(executor)::text, 'research_playbook_schedule', sqlc.arg(payload)::json, sqlc.arg(workspace_id)::text, 1,
        sqlc.arg(fire_key)::text, 'pending', (now() AT TIME ZONE 'UTC'), sqlc.arg(next_run_at)::timestamp, 3, 0)
ON CONFLICT (fire_key) WHERE status IN ('pending', 'processing') DO NOTHING
RETURNING id;

-- queue_service.add_job for a run (not single-flight).
-- name: PbInsertRunJob :one
INSERT INTO jobs (executor, type, payload, workspace_id, priority, fire_key, status,
                  created_at, next_run_at, max_retries, retry_count)
VALUES (sqlc.arg(executor)::text, 'research_playbook_run', sqlc.arg(payload)::json, sqlc.arg(workspace_id)::text, 1,
        sqlc.arg(fire_key)::text, 'pending', (now() AT TIME ZONE 'UTC'), (now() AT TIME ZONE 'UTC'), 3, 0)
RETURNING id;

-- name: PbCancelRunJobs :exec
UPDATE jobs SET status = 'cancelled', error = 'Cancelled by user'
WHERE workspace_id = sqlc.arg(workspace_id)::text AND type = 'research_playbook_run'
  AND fire_key = sqlc.arg(fire_key)::text AND status IN ('pending', 'processing');

-- name: PbInsertRun :one
INSERT INTO playbook_runs (id, workspace_id, playbook_id, audience_id, status, prompt_version, prompt_snapshot,
                           steps_snapshot, max_members, attempted, succeeded, failed, requested_by, created_at)
VALUES (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(playbook_id)::text, sqlc.arg(audience_id)::text,
        'pending', sqlc.arg(prompt_version)::int, sqlc.arg(prompt_snapshot)::text, sqlc.arg(steps_snapshot)::json,
        sqlc.arg(max_members)::int, 0, 0, 0, sqlc.arg(requested_by)::text, (now() AT TIME ZONE 'UTC'))
RETURNING id, playbook_id, audience_id, status, prompt_version, max_members, attempted, succeeded, failed,
          error, started_at, finished_at, created_at;

-- name: PbGetRun :one
SELECT id, playbook_id, audience_id, status, prompt_version, max_members, attempted, succeeded, failed,
       error, started_at, finished_at, created_at
FROM playbook_runs
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text;

-- name: PbListRuns :many
SELECT id, playbook_id, audience_id, status, prompt_version, max_members, attempted, succeeded, failed,
       error, started_at, finished_at, created_at
FROM playbook_runs
WHERE workspace_id = sqlc.arg(workspace_id)::text AND playbook_id = sqlc.arg(playbook_id)::text
  AND (NOT sqlc.arg(use_cursor)::bool
       OR created_at < sqlc.arg(cursor_created)::timestamp
       OR (created_at = sqlc.arg(cursor_created)::timestamp AND id < sqlc.arg(cursor_id)::text))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int OFFSET sqlc.arg(row_offset)::int;

-- name: PbRetryRun :one
UPDATE playbook_runs SET status = 'pending', error = NULL, finished_at = NULL
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text
  AND status IN ('failed', 'completed_with_errors', 'cancelled')
RETURNING id, playbook_id, audience_id, status, prompt_version, max_members, attempted, succeeded, failed,
          error, started_at, finished_at, created_at;

-- name: PbCancelRun :one
UPDATE playbook_runs SET status = 'cancelled', error = 'Cancelled by user', finished_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text
  AND status IN ('pending', 'running')
RETURNING id, playbook_id, audience_id, status, prompt_version, max_members, attempted, succeeded, failed,
          error, started_at, finished_at, created_at;

-- name: PbListResults :many
SELECT id, run_id, lead_id, status, value, result_metadata::text AS metadata, error, attempts, created_at, updated_at
FROM playbook_results
WHERE workspace_id = sqlc.arg(workspace_id)::text AND run_id = sqlc.arg(run_id)::text
  AND id > sqlc.arg(after_id)::bigint
ORDER BY id ASC
LIMIT sqlc.arg(row_limit)::int OFFSET sqlc.arg(row_offset)::int;

-- name: PbResultCounts :one
SELECT count(*)::int AS attempted,
       COALESCE(sum(CASE WHEN status = 'success' THEN 1 ELSE 0 END), 0)::int AS succeeded,
       COALESCE(sum(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0)::int AS failed
FROM playbook_results
WHERE workspace_id = sqlc.arg(workspace_id)::text AND run_id = sqlc.arg(run_id)::text;
