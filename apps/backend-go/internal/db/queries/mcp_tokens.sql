-- MCP tokens: only the SHA-256 hash is stored. Always filter by workspace_id.
-- Called inside tenancy.InWorkspace (the table itself carries no RLS policy).

-- name: InsertMCPToken :one
INSERT INTO mcp_tokens
    (id, token_hash, prefix, name, user_id, workspace_id, capabilities, expires_at, created_by)
VALUES
    (gen_random_uuid()::text, sqlc.arg(token_hash)::text, sqlc.arg(prefix)::text, sqlc.arg(name)::text,
     sqlc.arg(user_id)::int, sqlc.arg(workspace_id)::text, sqlc.arg(capabilities)::json,
     sqlc.narg(expires_at)::timestamptz, sqlc.arg(created_by)::int)
RETURNING *;

-- name: ListMCPTokens :many
SELECT * FROM mcp_tokens
WHERE workspace_id = sqlc.arg(workspace_id)::text
ORDER BY created_at DESC, id;

-- name: GetMCPToken :one
SELECT * FROM mcp_tokens
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: RevokeMCPToken :exec
UPDATE mcp_tokens SET revoked_at = now()
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text AND revoked_at IS NULL;
