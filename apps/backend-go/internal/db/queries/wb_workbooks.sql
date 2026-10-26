-- Workbook store for the CRUD service (wbcrud). Every statement runs inside
-- tenancy.InWorkspace (FORCE RLS) and also carries the explicit workspace_id
-- predicate. Every UPDATE that SQLAlchemy stamps with onupdate=func.now()
-- sets updated_at = now() at time zone 'utc'. Row counts reuse
-- CountWorkbookRows (workbook.sql).

-- name: GetWorkbookOwned :one
SELECT workbooks.* FROM workbooks
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: GetWorkbookOwnedForUpdate :one
SELECT workbooks.* FROM workbooks
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
FOR UPDATE;

-- name: ListWorkbooks :many
SELECT workbooks.* FROM workbooks
WHERE workspace_id = sqlc.arg(workspace_id)::text
ORDER BY updated_at DESC, id;

-- name: InsertWorkbook :one
INSERT INTO workbooks (id, name, description, status, workspace_id, source_type, source_config,
                       filter_criteria, columns_config, total_rows, completed_rows, sync_to_leads,
                       budget_max_usd, budget_spent_usd, refresh_policy, created_at, updated_at)
VALUES (sqlc.arg(id)::text, sqlc.arg(name)::text, sqlc.arg(description)::text, 'draft',
        sqlc.arg(workspace_id)::text, sqlc.arg(source_type)::text, sqlc.arg(source_config)::json,
        sqlc.arg(filter_criteria)::json, sqlc.arg(columns_config)::json, 0, 0, true,
        0.0, 0.0, '{}'::json, (now() AT TIME ZONE 'utc'), (now() AT TIME ZONE 'utc'))
RETURNING workbooks.*;

-- name: UpdateWorkbookMeta :one
-- NULL name/description/status keep the stored value; set_filter_criteria
-- replaces filter_criteria (PUT semantics of WorkbookUpdate).
UPDATE workbooks SET
  name = COALESCE(sqlc.narg(name)::text, name),
  description = COALESCE(sqlc.narg(description)::text, description),
  status = COALESCE(sqlc.narg(status)::text, status),
  filter_criteria = CASE WHEN sqlc.arg(set_filter_criteria)::bool THEN sqlc.narg(filter_criteria)::json ELSE filter_criteria END,
  updated_at = (now() AT TIME ZONE 'utc')
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
RETURNING workbooks.*;

-- name: UpdateWorkbookColumns :one
UPDATE workbooks SET columns_config = sqlc.arg(columns_config)::json, updated_at = (now() AT TIME ZONE 'utc')
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
RETURNING workbooks.*;

-- name: DeleteWorkbook :execrows
DELETE FROM workbooks
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: TouchWorkbook :exec
UPDATE workbooks SET updated_at = (now() AT TIME ZONE 'utc')
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;
