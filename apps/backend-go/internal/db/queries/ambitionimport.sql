-- AmbitionBox import job (plan 07 task 13). Every statement runs inside
-- tenancy.InWorkspace (FORCE RLS) and also carries the explicit workspace predicate.

-- name: AIWorkbookForStart :one
-- Row lock: serialises starts for one workbook across replicas.
SELECT id, workspace_id, name, source_type FROM workbooks WHERE id = sqlc.arg(workbook_id)::text FOR UPDATE;

-- name: AIActiveRun :one
SELECT id FROM connector_runs
WHERE workbook_id = sqlc.arg(workbook_id)::text AND connector = 'ambitionbox'
  AND status IN ('pending', 'running', 'retrying')
ORDER BY created_at DESC LIMIT 1;

-- name: AIRepurposeWorkbook :exec
UPDATE workbooks SET
  name = CASE WHEN sqlc.arg(name)::text = '' THEN name ELSE sqlc.arg(name)::text END,
  columns_config = sqlc.arg(columns_config)::text::json,
  source_type = 'ambitionbox', status = 'running',
  updated_at = (now() AT TIME ZONE 'utc')
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AIInsertWorkbook :one
INSERT INTO workbooks (id, name, description, status, workspace_id, source_type, source_config,
                       filter_criteria, columns_config, total_rows, completed_rows, sync_to_leads,
                       budget_max_usd, budget_spent_usd, refresh_policy, created_at, updated_at)
VALUES (sqlc.arg(id)::text, sqlc.arg(name)::text, sqlc.arg(description)::text, 'running',
        sqlc.arg(workspace_id)::text, 'ambitionbox', '{}'::json,
        '{}'::json, sqlc.arg(columns_config)::text::json, 0, 0, false,
        0.0, 0.0, '{}'::json, (now() AT TIME ZONE 'utc'), (now() AT TIME ZONE 'utc'))
RETURNING name;

-- name: AIInsertRun :exec
INSERT INTO connector_runs (id, workspace_id, workbook_id, connector, status, query, cursor, requested_count,
                            fetched_count, added_count, updated_count, skipped_count, pages_fetched,
                            target_met, exhausted, created_at, updated_at)
VALUES (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(workbook_id)::text, 'ambitionbox', 'pending',
        sqlc.arg(query)::text::json, sqlc.arg(cursor)::text::json, sqlc.arg(requested_count)::int,
        0, 0, 0, 0, 0, false, false, now(), now());

-- name: AISetSourceConfig :exec
UPDATE workbooks SET source_config = sqlc.arg(source_config)::text::json, updated_at = (now() AT TIME ZONE 'utc')
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AIInsertJob :one
-- queue_service.add_job(priority=2, executor=...).
INSERT INTO jobs (executor, type, payload, workspace_id, priority, fire_key, status,
                  created_at, next_run_at, max_retries, retry_count)
VALUES (sqlc.arg(executor)::text, 'ambitionbox_import', sqlc.arg(payload)::text::json,
        NULLIF(sqlc.arg(workspace_id)::text, ''), 2, NULL, 'pending',
        (now() AT TIME ZONE 'UTC'), (now() AT TIME ZONE 'UTC'), 3, 0)
RETURNING id;

-- name: AILoadRun :one
-- Row lock: two workers (or a retry) never advance the same page.
SELECT id, workspace_id, workbook_id, status, coalesce(query::text, '{}')::text AS query, coalesce(cursor::text, '{}')::text AS cursor,
       requested_count, pages_fetched, added_count, updated_count, skipped_count,
       target_met, exhausted, error
FROM connector_runs
WHERE id = sqlc.arg(run_id)::text AND workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
FOR UPDATE;

-- name: AILoadWorkbook :one
SELECT status, source_config::text AS source_config FROM workbooks
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
FOR UPDATE;

-- name: AIMarkRunning :exec
UPDATE connector_runs SET status = 'running', error = NULL, completed_at = NULL,
  started_at = COALESCE(started_at, now()), updated_at = now()
WHERE id = sqlc.arg(run_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AISetWorkbookState :exec
UPDATE workbooks SET status = sqlc.arg(status)::text, source_config = sqlc.arg(source_config)::text::json,
  total_rows = COALESCE(sqlc.narg(total_rows)::int, total_rows), updated_at = (now() AT TIME ZONE 'utc')
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AIUpdateRunPage :exec
UPDATE connector_runs SET
  status = sqlc.arg(status)::text, cursor = sqlc.arg(cursor)::text::json,
  pages_fetched = sqlc.arg(pages_fetched)::int, fetched_count = sqlc.arg(fetched_count)::int,
  added_count = sqlc.arg(added_count)::int, updated_count = sqlc.arg(updated_count)::int,
  skipped_count = sqlc.arg(skipped_count)::int, source_total = sqlc.narg(source_total)::int,
  target_met = sqlc.arg(target_met)::bool, exhausted = sqlc.arg(exhausted)::bool,
  error = sqlc.narg(error)::text,
  completed_at = CASE WHEN sqlc.arg(done)::bool THEN now() ELSE NULL END,
  updated_at = now()
WHERE id = sqlc.arg(run_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AICountRows :one
SELECT count(*)::int AS total,
       count(*) FILTER (WHERE source_provider = 'ambitionbox')::int AS ambitionbox
FROM record_workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AISetRunOutcome :execrows
-- Failure path of the handler and the reconcile hook: never touches a finished run.
UPDATE connector_runs SET status = sqlc.arg(status)::text, error = sqlc.arg(error)::text,
  completed_at = CASE WHEN sqlc.arg(terminal)::bool THEN now() ELSE NULL END, updated_at = now()
WHERE id = sqlc.arg(run_id)::text AND workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
  AND status NOT IN ('complete', 'partial');
