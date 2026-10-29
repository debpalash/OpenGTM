-- Workbook telemetry reads and the budget write (wbcrud/telemetry.go). Run inside
-- tenancy.InWorkspace; every statement also carries the explicit workspace_id predicate.

-- name: GetBudgetFields :one
SELECT budget_max_usd, budget_spent_usd FROM workbooks
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: SetBudgetIfChanged :execrows
-- Writes (and stamps updated_at) only when the value really changes, like an ORM
-- attribute assignment: a no-op assignment emits no UPDATE.
UPDATE workbooks SET budget_max_usd = sqlc.arg(max_usd)::float8, updated_at = (now() AT TIME ZONE 'utc')
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
  AND budget_max_usd IS DISTINCT FROM sqlc.arg(max_usd)::float8;

-- name: SumSpendExposure :many
SELECT status::text AS status, sum(reserved_microusd)::bigint AS micro
FROM workbook_spend_attempts
WHERE workspace_id = sqlc.arg(workspace_id)::text AND workbook_id = sqlc.arg(workbook_id)::text
  AND status IN ('reserved', 'dispatched', 'uncertain')
GROUP BY status;

-- name: ListRunWorkbookJobs :many
-- jobs.payload is json: a non-object payload never matches (->> on a scalar raises).
SELECT id, payload, status, created_at, started_at, completed_at, last_heartbeat, next_run_at, error, retry_count
FROM jobs
WHERE workspace_id = sqlc.arg(workspace_id)::text AND type = 'run_workbook'
  AND CASE WHEN json_typeof(payload) = 'object' THEN payload ->> 'workbook_id' END = sqlc.arg(workbook_id)::text
  AND (sqlc.narg(before_id)::int IS NULL OR id < sqlc.narg(before_id)::int)
ORDER BY id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: ListWorkbookActivity :many
SELECT id, kind, message, created_at FROM workbook_activity
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: GetCellTrace :one
SELECT workbook_id, lead_id, column_id, goal, steps, outcome, total_cost_usd FROM cell_traces
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
  AND lead_id = sqlc.arg(lead_id)::int AND column_id = sqlc.arg(column_id)::text
ORDER BY id LIMIT 1;
