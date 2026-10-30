-- Playbook run engine (internal/playbooks/engine.go): the research_playbook_run
-- handler and its failure reconcile. playbook_runs, playbook_results and
-- audience_members are FORCE RLS (run inside tenancy.InWorkspace) and every
-- statement filters by workspace_id as well. Timestamps are naive UTC columns.

-- name: PbEngLockRun :one
SELECT id, playbook_id, audience_id, status, prompt_snapshot, steps_snapshot, max_members, started_at, error
FROM playbook_runs
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text
FOR UPDATE;

-- name: PbEngStartRun :execrows
UPDATE playbook_runs
SET status = 'running', started_at = COALESCE(started_at, (now() AT TIME ZONE 'UTC')), error = NULL
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text
  AND status NOT IN ('completed', 'cancelled');

-- name: PbEngRunStatus :one
SELECT status FROM playbook_runs
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text;

-- name: PbEngMembers :many
SELECT lead_id, company_entity_id,person_entity_id,snapshot::text AS snapshot
FROM audience_members
WHERE workspace_id = sqlc.arg(workspace_id)::text AND audience_id = sqlc.arg(audience_id)::text
ORDER BY lead_id,id
LIMIT sqlc.arg(row_limit)::int;

-- A result already success yields no row (skipped on retry); otherwise it turns
-- running with attempts + 1 and a cleared error.
-- name: PbEngBeginResult :one
INSERT INTO playbook_results (workspace_id, run_id, lead_id, company_entity_id,person_entity_id,subject_key,status, value, result_metadata, attempts, created_at, updated_at)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(run_id)::text, sqlc.narg(lead_id)::int,sqlc.narg(company_id)::text,sqlc.narg(person_id)::text,sqlc.arg(subject_key)::text, 'running', '', '{}', 1,
        (now() AT TIME ZONE 'UTC'), (now() AT TIME ZONE 'UTC'))
ON CONFLICT (workspace_id, run_id, subject_key) DO UPDATE
SET status = 'running', attempts = playbook_results.attempts + 1, error = NULL, updated_at = (now() AT TIME ZONE 'UTC')
WHERE playbook_results.status <> 'success'
RETURNING id, attempts;

-- name: PbEngFinishResult :execrows
UPDATE playbook_results
SET status = sqlc.arg(status)::text, value = sqlc.arg(value)::text, result_metadata = sqlc.arg(metadata)::json,
    error = sqlc.narg(error)::text, updated_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::int AND status = 'running';

-- name: PbEngFailResult :execrows
UPDATE playbook_results
SET status = 'failed', error = sqlc.arg(error)::text, updated_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::int AND status = 'running';

-- name: PbEngCancelResult :execrows
UPDATE playbook_results
SET status = 'cancelled', error = 'Cancelled by user', updated_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::int AND status = 'running';

-- name: PbEngRevertResult :execrows
UPDATE playbook_results
SET status = 'pending', attempts = GREATEST(attempts - 1, 0), updated_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::int AND status = 'running';

-- Puts a still-running run back (worker unavailable): only while it is running,
-- so a cancel that raced in is never undone.
-- name: PbEngRestoreRun :execrows
UPDATE playbook_runs
SET status = sqlc.arg(status)::text,
    started_at = CASE WHEN sqlc.arg(keep_started)::bool THEN started_at ELSE sqlc.narg(started_at)::timestamp END,
    error = sqlc.narg(error)::text,
    finished_at = NULL
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text AND status = 'running';

-- Final counts and status; never overwrites a run that is no longer running
-- (cancelled meanwhile).
-- name: PbEngFinishRun :execrows
UPDATE playbook_runs
SET attempted = sqlc.arg(attempted)::int, succeeded = sqlc.arg(succeeded)::int, failed = sqlc.arg(failed)::int,
    status = CASE WHEN sqlc.arg(failed)::int = 0 THEN 'completed' ELSE 'completed_with_errors' END,
    finished_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text AND status = 'running';

-- name: PbEngReconcileRetryRun :exec
UPDATE playbook_runs SET status = 'pending', error = sqlc.arg(error)::text, finished_at = NULL
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text;

-- name: PbEngReconcileRetryResults :exec
UPDATE playbook_results SET status = 'pending', error = sqlc.arg(error)::text, updated_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND run_id = sqlc.arg(run_id)::text AND status = 'running';

-- name: PbEngReconcileFailResults :exec
UPDATE playbook_results SET status = 'failed', error = sqlc.arg(error)::text, updated_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND run_id = sqlc.arg(run_id)::text
  AND status IN ('pending', 'running');

-- name: PbEngReconcileFailRun :exec
UPDATE playbook_runs
SET status = 'failed', error = sqlc.arg(error)::text, finished_at = (now() AT TIME ZONE 'UTC'),
    attempted = sqlc.arg(attempted)::int, succeeded = sqlc.arg(succeeded)::int, failed = sqlc.arg(failed)::int
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text;
