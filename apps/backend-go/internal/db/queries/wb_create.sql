-- Workbook create / from-jobs / update support (Task 9). Every statement runs
-- inside tenancy.InWorkspace (FORCE RLS) and carries the explicit workspace_id.

-- name: InsertWorkbookRows :many
-- Bulk snapshot insert from one jsonb array of {"p": position, "l": lead id or
-- 0, "d": row data as a JSON *string*} (the string keeps the data column's key
-- order, which jsonb would not). Rows are inserted in array order.
-- corroboration_count mirrors the ORM default (1); timestamps use the table
-- defaults like the ORM's server_default=now().
INSERT INTO workbook_rows (workbook_id, workspace_id, position, data, enrichments, lead_id, corroboration_count)
SELECT sqlc.arg(workbook_id)::text, sqlc.arg(workspace_id)::text, (e.value->>'p')::int,
       (e.value->>'d')::json, '{}'::json, NULLIF((e.value->>'l')::int, 0), 1
FROM jsonb_array_elements(sqlc.arg(items)::jsonb) WITH ORDINALITY AS e(value, ord)
ORDER BY e.ord
RETURNING id;

-- name: ListWorkbookRowLeadIDs :many
SELECT lead_id::int AS lead_id FROM workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text AND lead_id IS NOT NULL;

-- name: ListWorkbookViewConfigs :many
-- Saved views of a workbook, for the replace-columns guard.
SELECT name, config FROM workbook_views
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
ORDER BY created_at, id;
