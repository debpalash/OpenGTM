-- workspace_settings is a tenant table under FORCE RLS: run these inside
-- tenancy.InWorkspace. workspace_id is always passed explicitly as well.

-- name: GetWorkspaceSetting :one
SELECT value FROM workspace_settings
WHERE workspace_id = sqlc.arg(workspace_id)::text AND key = sqlc.arg(key)::text;

-- name: UpsertWorkspaceSetting :exec
INSERT INTO workspace_settings (workspace_id, key, value)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(key)::text, sqlc.arg(value)::text)
ON CONFLICT (workspace_id, key) DO UPDATE SET value = EXCLUDED.value;

-- name: ListWorkspaceSettingKeys :many
SELECT key FROM workspace_settings
WHERE workspace_id = sqlc.arg(workspace_id)::text AND value <> ''
ORDER BY key;
