-- Queries for internal/runplan (Task 19): the exact row set a run covers.
-- Every statement runs inside tenancy.InWorkspace (FORCE RLS) and also carries
-- the explicit workbook/workspace predicate. Order is position then id; Python
-- reads these rows without an ORDER BY, so the order is only made deterministic.
-- Saved-view and search resolution is the dynamic query of wbcrud.BuildRowQuery.

-- name: RunplanRowIDs :many
SELECT id::bigint AS id
FROM workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
ORDER BY coalesce("position", 0), id;

-- name: RunplanRowIDsByRowIDs :many
SELECT id::bigint AS id
FROM workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
  AND id = ANY(sqlc.arg(row_ids)::bigint[])
ORDER BY coalesce("position", 0), id;

-- name: RunplanRowIDsByLeadIDs :many
SELECT id::bigint AS id
FROM workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
  AND lead_id = ANY(sqlc.arg(lead_ids)::bigint[])
ORDER BY coalesce("position", 0), id;
