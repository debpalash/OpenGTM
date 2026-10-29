-- Governance IdP: SCIM bearer token management (routers/governance.py scim-token).
-- workspace_scim_tokens is identity-plane (no RLS); every statement is keyed by workspace_id.
-- OIDC config reuses GetWorkspaceSetting / UpsertWorkspaceSetting (workspace_settings.sql).

-- name: GovIdpTokenStatus :one
SELECT token_prefix, created_at, expires_at, last_used_at, created_by
FROM workspace_scim_tokens WHERE workspace_id = sqlc.arg(workspace_id)::text;

-- name: GovIdpRotateToken :exec
INSERT INTO workspace_scim_tokens (workspace_id, token_hash, token_prefix, created_at, expires_at, last_used_at, created_by)
VALUES (
  sqlc.arg(workspace_id)::text, sqlc.arg(token_hash)::text, sqlc.arg(token_prefix)::text,
  sqlc.arg(created_at)::float8, sqlc.arg(expires_at)::float8, NULL, sqlc.arg(created_by)::int
)
ON CONFLICT (workspace_id) DO UPDATE SET
  token_hash = EXCLUDED.token_hash, token_prefix = EXCLUDED.token_prefix,
  created_at = EXCLUDED.created_at, expires_at = EXCLUDED.expires_at,
  last_used_at = NULL, created_by = EXCLUDED.created_by;

-- name: GovIdpRevokeToken :exec
DELETE FROM workspace_scim_tokens WHERE workspace_id = sqlc.arg(workspace_id)::text;
