-- Saved views (wbcrud/viewsvc.go). Run inside tenancy.InWorkspace; every
-- statement also carries the explicit workspace_id predicate.

-- name: ListWorkbookViews :many
SELECT workbook_views.* FROM workbook_views
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
ORDER BY created_at, id;

-- name: GetWorkbookViewOwned :one
SELECT workbook_views.* FROM workbook_views
WHERE id = sqlc.arg(view_id)::text
  AND workbook_id = sqlc.arg(workbook_id)::text
  AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: InsertWorkbookView :one
INSERT INTO workbook_views (id, workspace_id, workbook_id, name, config, created_at, updated_at)
VALUES (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(workbook_id)::text,
        sqlc.arg(name)::text, sqlc.arg(config)::json,
        (now() AT TIME ZONE 'utc'), (now() AT TIME ZONE 'utc'))
RETURNING workbook_views.*;

-- name: UpdateWorkbookView :one
-- NULL name / unset config keep the stored value; updated_at is always stamped.
UPDATE workbook_views SET
  name = COALESCE(sqlc.narg(name)::text, name),
  config = CASE WHEN sqlc.arg(set_config)::bool THEN sqlc.narg(config)::json ELSE config END,
  updated_at = (now() AT TIME ZONE 'utc')
WHERE id = sqlc.arg(view_id)::text
  AND workbook_id = sqlc.arg(workbook_id)::text
  AND workspace_id = sqlc.arg(workspace_id)::text
RETURNING workbook_views.*;

-- name: DeleteWorkbookView :execrows
DELETE FROM workbook_views
WHERE id = sqlc.arg(view_id)::text
  AND workbook_id = sqlc.arg(workbook_id)::text
  AND workspace_id = sqlc.arg(workspace_id)::text;
