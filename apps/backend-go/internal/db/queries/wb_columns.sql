-- Column route guards (wbcrud/columnsvc.go). Run inside tenancy.InWorkspace.

-- name: ListWorkbookViewsForColumnGuard :many
-- Python lists views with .all() (heap order, unspecified); created_at, id
-- makes the "referenced by saved views" names deterministic.
SELECT name, config FROM workbook_views
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
ORDER BY created_at, id;

-- name: DeleteWorkbookEnrichmentsForColumn :exec
DELETE FROM workbook_enrichments
WHERE workbook_id = sqlc.arg(workbook_id)::text
  AND workspace_id = sqlc.arg(workspace_id)::text
  AND column_id = sqlc.arg(column_id)::text;
