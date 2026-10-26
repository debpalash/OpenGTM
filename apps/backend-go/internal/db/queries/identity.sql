-- users is identity-plane (no RLS): run these on the plain pool.

-- name: UserByUsername :one
SELECT * FROM users WHERE username = sqlc.arg(username)::text;

-- name: UserByID :one
SELECT * FROM users WHERE id = sqlc.arg(id)::int;

-- name: ListUsers :many
SELECT * FROM users ORDER BY id;

-- name: InsertUser :one
INSERT INTO users (username, hashed_password, is_active, is_admin, role, profile_image, created_at)
VALUES (
  sqlc.arg(username)::text, sqlc.arg(hashed_password)::text, sqlc.arg(is_active)::bool,
  sqlc.arg(is_admin)::bool, sqlc.arg(role)::text, sqlc.narg(profile_image)::text,
  now() at time zone 'utc'
)
RETURNING *;

-- name: UpdateUserPassword :exec
UPDATE users SET hashed_password = sqlc.arg(hashed_password)::text WHERE id = sqlc.arg(id)::int;

-- name: UpdateUserRole :exec
UPDATE users SET role = sqlc.arg(role)::text, is_admin = sqlc.arg(is_admin)::bool WHERE id = sqlc.arg(id)::int;

-- name: DeleteUser :exec
DELETE FROM users WHERE id = sqlc.arg(id)::int;

-- name: TouchLastLogin :exec
UPDATE users SET last_login = now() at time zone 'utc' WHERE id = sqlc.arg(id)::int;

-- Workspace resolution. workspaces, workspace_members and user_active_workspace
-- are identity-plane (plain pool, no GUC). workspace_member_permissions and
-- workspace_settings are FORCE RLS: run those inside tenancy.InWorkspace.

-- name: WorkspaceByID :one
SELECT * FROM workspaces WHERE id = sqlc.arg(id)::text;

-- name: WorkspaceBySlug :one
SELECT * FROM workspaces WHERE slug = sqlc.arg(slug)::text;

-- name: MemberRole :one
SELECT role FROM workspace_members
WHERE workspace_id = sqlc.arg(workspace_id)::text AND user_id = sqlc.arg(user_id)::int;

-- name: IsMember :one
SELECT EXISTS (
  SELECT 1 FROM workspace_members
  WHERE workspace_id = sqlc.arg(workspace_id)::text AND user_id = sqlc.arg(user_id)::int
  UNION
  SELECT 1 FROM workspaces
  WHERE id = sqlc.arg(workspace_id)::text AND owner_id = sqlc.arg(user_id)::int
)::bool AS is_member;

-- name: ActiveWorkspace :one
SELECT workspace_id FROM user_active_workspace WHERE user_id = sqlc.arg(user_id)::int;

-- name: FirstAccessibleWorkspace :one
SELECT id::text AS id FROM (
  SELECT workspace_id AS id FROM workspace_members WHERE user_id = sqlc.arg(user_id)::int
  UNION
  SELECT id FROM workspaces WHERE owner_id = sqlc.arg(user_id)::int
) a ORDER BY id LIMIT 1;

-- name: MemberPermissions :many
SELECT permission, effect FROM workspace_member_permissions
WHERE workspace_id = sqlc.arg(workspace_id)::text AND user_id = sqlc.arg(user_id)::int;

-- name: OIDCConfig :one
SELECT value FROM workspace_settings
WHERE workspace_id = sqlc.arg(workspace_id)::text AND key = 'OIDC_CONFIG';
