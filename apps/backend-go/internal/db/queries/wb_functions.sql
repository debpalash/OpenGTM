-- Reusable workbook functions. workbook_functions is a global table (no RLS, user
-- decision), so these run on the plain pool; the two workbooks statements run
-- inside tenancy.InWorkspace.

-- name: ListWorkbookFunctions :many
SELECT id, name, description, category, columns_chain, version, usage_count, created_at
FROM workbook_functions
WHERE (sqlc.arg(category)::text = '' OR category = sqlc.arg(category)::text)
ORDER BY usage_count DESC, created_at, id;

-- name: GetWorkbookFunction :one
SELECT id, name, description, category, columns_chain, version, usage_count
FROM workbook_functions
WHERE id = sqlc.arg(id)::text;

-- name: InsertWorkbookFunction :exec
INSERT INTO workbook_functions (id, name, description, category, columns_chain, version, usage_count, created_at, updated_at)
VALUES (sqlc.arg(id)::text, sqlc.arg(name)::text, sqlc.arg(description)::text, sqlc.arg(category)::text,
        sqlc.arg(columns_chain)::jsonb, 1, 0, sqlc.arg(now)::float8, sqlc.arg(now)::float8);

-- name: DeleteWorkbookFunction :exec
DELETE FROM workbook_functions WHERE id = sqlc.arg(id)::text;

-- name: BumpWorkbookFunctionUsage :exec
UPDATE workbook_functions SET usage_count = usage_count + 1 WHERE id = sqlc.arg(id)::text;

-- name: LockWorkbookColumnsConfig :one
SELECT columns_config FROM workbooks
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
FOR UPDATE;

-- name: SetWorkbookColumnsConfig :exec
UPDATE workbooks SET columns_config = sqlc.arg(columns_config)::json, updated_at = (now() AT TIME ZONE 'utc')
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;
