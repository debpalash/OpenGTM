-- MCP token authentication (plan 13, task 5). mcp_tokens and the identity-plane
-- tables carry no RLS; the token is always looked up by its sha256 hex.

-- name: McpAuthTokenByHash :one
SELECT id, token_hash, user_id, workspace_id, capabilities, expires_at, revoked_at
FROM mcp_tokens
WHERE token_hash = sqlc.arg(token_hash)::text;

-- name: McpAuthTouchToken :exec
UPDATE mcp_tokens SET last_used_at = now() WHERE id = sqlc.arg(id)::text;

-- name: McpAuthWorkspaceSlug :one
SELECT slug FROM workspaces WHERE id = sqlc.arg(id)::text;

-- name: McpAuthMainWorkspaceID :one
SELECT id FROM workspaces WHERE slug = 'main';
