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

-- Workspace management (/api/workspaces). All identity-plane except leads and the
-- RLS child tables, which run inside tenancy.InWorkspace.

-- name: ListWorkspacesForUser :many
SELECT w.* FROM workspaces w
WHERE w.id IN (
  SELECT workspace_id FROM workspace_members WHERE user_id = sqlc.arg(user_id)::int
  UNION
  SELECT id FROM workspaces WHERE owner_id = sqlc.arg(user_id)::int
)
ORDER BY w.created_at ASC, w.id;

-- name: InsertWorkspace :one
INSERT INTO workspaces (id, name, slug, description, icon, owner_id, created_at, updated_at)
VALUES (
  sqlc.arg(id)::text, sqlc.arg(name)::text, sqlc.arg(slug)::text, sqlc.arg(description)::text,
  sqlc.arg(icon)::text, sqlc.arg(owner_id)::int, sqlc.arg(now)::float8, sqlc.arg(now)::float8
)
RETURNING *;

-- name: UpsertOwnerMember :exec
INSERT INTO workspace_members (workspace_id, user_id, role, created_at)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(user_id)::int, 'owner', sqlc.arg(now)::float8)
ON CONFLICT (workspace_id, user_id) DO UPDATE
SET role = EXCLUDED.role, created_at = EXCLUDED.created_at;

-- name: SetActiveWorkspace :exec
INSERT INTO user_active_workspace (user_id, workspace_id)
VALUES (sqlc.arg(user_id)::int, sqlc.arg(workspace_id)::text)
ON CONFLICT (user_id) DO UPDATE SET workspace_id = EXCLUDED.workspace_id;

-- name: CountLeads :one
SELECT count(*)::bigint FROM leads;

-- Delete children in manager.delete_workspace order (RLS tables need the GUC).
-- name: DeleteWorkspaceSettings :exec
DELETE FROM workspace_settings WHERE workspace_id = sqlc.arg(workspace_id)::text;
-- name: DeleteWorkspaceOIDCIdentities :exec
DELETE FROM workspace_oidc_identities WHERE workspace_id = sqlc.arg(workspace_id)::text;
-- name: DeleteWorkspaceSCIMTokens :exec
DELETE FROM workspace_scim_tokens WHERE workspace_id = sqlc.arg(workspace_id)::text;
-- name: DeleteWorkspaceSCIMUsers :exec
DELETE FROM workspace_scim_users WHERE workspace_id = sqlc.arg(workspace_id)::text;
-- name: DeleteWorkspaceSCIMGroupMembers :exec
DELETE FROM workspace_scim_group_members WHERE workspace_id = sqlc.arg(workspace_id)::text;
-- name: DeleteWorkspaceSCIMGroups :exec
DELETE FROM workspace_scim_groups WHERE workspace_id = sqlc.arg(workspace_id)::text;
-- name: DeleteWorkspaceMemberPermissions :exec
DELETE FROM workspace_member_permissions WHERE workspace_id = sqlc.arg(workspace_id)::text;
-- name: DeleteWorkspaceMembers :exec
DELETE FROM workspace_members WHERE workspace_id = sqlc.arg(workspace_id)::text;
-- name: DeleteWorkspaceActive :exec
DELETE FROM user_active_workspace WHERE workspace_id = sqlc.arg(workspace_id)::text;
-- name: DeleteWorkspaceRow :exec
DELETE FROM workspaces WHERE id = sqlc.arg(workspace_id)::text;

-- First-admin setup (POST /auth/setup). All identity-plane.

-- name: LockFirstAdminSetup :exec
SELECT pg_advisory_xact_lock(hashtext('opengtm:first-admin-setup'));

-- name: AnyUser :one
SELECT EXISTS (SELECT 1 FROM users);

-- name: MainWorkspaceID :one
SELECT id FROM workspaces WHERE slug = 'main';

-- ensure_tenancy_backfill, step 1: ownerless workspaces belong to the first admin.
-- name: ClaimOwnerlessWorkspaces :exec
UPDATE workspaces SET owner_id = sqlc.arg(owner_id)::int WHERE owner_id IS NULL;

-- ensure_tenancy_backfill, step 2: the first admin owns every existing workspace.
-- name: AddOwnerToAllWorkspaces :exec
INSERT INTO workspace_members (workspace_id, user_id, role, created_at)
SELECT id, sqlc.arg(user_id)::int, 'owner', sqlc.arg(now)::float8 FROM workspaces
ON CONFLICT (workspace_id, user_id) DO NOTHING;
