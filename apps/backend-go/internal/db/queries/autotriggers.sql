-- plan 08 (automations) task 13: rule CRUD, run history and manual-run support
-- for internal/autosvc. Every statement runs inside tenancy.InWorkspace (the
-- tables are FORCE RLS; the workspace_id predicate is belt and braces).

-- name: AutoSvcCountTriggers :one
SELECT count(*)::bigint AS n FROM triggers WHERE workspace_id = sqlc.arg(workspace_id)::text;

-- name: AutoSvcScopedColumns :many
-- columns_config of the rule's scoped workbooks (all workspace workbooks when
-- the scope is empty), as _scoped_columns reads them.
SELECT columns_config FROM workbooks
WHERE workspace_id = sqlc.arg(workspace_id)::text
  AND (cardinality(sqlc.arg(ids)::text[]) = 0 OR id = ANY(sqlc.arg(ids)::text[]));

-- name: AutoSvcInsertTrigger :one
INSERT INTO triggers (id, workspace_id, name, enabled, trigger_type, trigger_config, "condition", actions,
                      scope_workbook_ids, stop_on_error, max_spend_usd_per_day, max_actions_per_day,
                      schedule_anchor, created_by)
VALUES (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(name)::text, sqlc.arg(enabled)::boolean,
        sqlc.arg(trigger_type)::text, sqlc.arg(trigger_config)::json, sqlc.arg(condition)::text,
        sqlc.arg(actions)::json, sqlc.arg(scope_workbook_ids)::json, sqlc.arg(stop_on_error)::boolean,
        sqlc.narg(max_spend_usd_per_day)::float8, sqlc.narg(max_actions_per_day)::int4,
        sqlc.narg(schedule_anchor)::timestamptz, sqlc.narg(created_by)::text)
RETURNING *;

-- name: AutoSvcListTriggers :many
SELECT * FROM triggers
WHERE workspace_id = sqlc.arg(workspace_id)::text
  AND (sqlc.narg(enabled)::boolean IS NULL OR enabled = sqlc.narg(enabled)::boolean)
  AND (sqlc.narg(trigger_type)::text IS NULL OR trigger_type = sqlc.narg(trigger_type)::text)
ORDER BY created_at DESC, id DESC;

-- name: AutoSvcGetTrigger :one
SELECT * FROM triggers WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AutoSvcLockTrigger :one
-- Row-locked read for patch, pause and resume (read-modify-write).
SELECT * FROM triggers WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text FOR UPDATE;

-- name: AutoSvcUpdateTrigger :one
UPDATE triggers
SET name = sqlc.arg(name)::text, enabled = sqlc.arg(enabled)::boolean,
    trigger_config = sqlc.arg(trigger_config)::json, "condition" = sqlc.arg(condition)::text,
    actions = sqlc.arg(actions)::json, scope_workbook_ids = sqlc.arg(scope_workbook_ids)::json,
    stop_on_error = sqlc.arg(stop_on_error)::boolean,
    max_spend_usd_per_day = sqlc.narg(max_spend_usd_per_day)::float8,
    max_actions_per_day = sqlc.narg(max_actions_per_day)::int4, updated_at = now()
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
RETURNING *;

-- name: AutoSvcSetEnabled :execrows
UPDATE triggers SET enabled = sqlc.arg(enabled)::boolean, updated_at = now()
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AutoSvcDeleteTrigger :execrows
DELETE FROM triggers WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AutoSvcRecentRuns :many
SELECT * FROM trigger_runs
WHERE workspace_id = sqlc.arg(workspace_id)::text AND trigger_id = sqlc.arg(trigger_id)::text
ORDER BY started_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: AutoSvcGetRun :one
SELECT * FROM trigger_runs WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AutoSvcRunResults :many
SELECT * FROM trigger_action_results
WHERE workspace_id = sqlc.arg(workspace_id)::text AND run_id = sqlc.arg(run_id)::text
ORDER BY id ASC;

-- name: AutoSvcRowWorkbooks :many
-- Workbook of each requested row id (PreviewTargets / manual run target
-- resolution). Rows of other workspaces are invisible (RLS plus predicate).
SELECT id::int4 AS row_id, workbook_id FROM workbook_rows
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = ANY(sqlc.arg(ids)::int4[]);

-- name: AutoSvcInsertEvalJob :one
-- queue_service.add_job(priority=1, executor=trigger_eval_executor(...)); a
-- manual run has no fire_key column value (the key lives in the payload).
INSERT INTO jobs (executor, type, payload, workspace_id, priority, status,
                  created_at, next_run_at, max_retries, retry_count)
VALUES (sqlc.arg(executor)::text, 'trigger_eval', sqlc.arg(payload)::json, sqlc.arg(workspace_id)::text, 1, 'pending',
        (now() AT TIME ZONE 'UTC'), (now() AT TIME ZONE 'UTC'), 3, 0)
RETURNING id;
