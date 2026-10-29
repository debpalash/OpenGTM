-- SCIM (/scim/v2/{slug}). workspace_scim_tokens is identity-plane (plain pool).
-- workspace_scim_users/groups/group_members and workspace_member_permissions are
-- FORCE RLS: run them inside tenancy.InWorkspace. workspace_members and
-- workspace_oidc_identities are identity-plane but are written in the same tx.

-- name: ScimTokenRow :one
SELECT token_hash, created_at, expires_at, last_used_at
FROM workspace_scim_tokens WHERE workspace_id = sqlc.arg(workspace_id)::text;

-- name: ScimTouchToken :exec
UPDATE workspace_scim_tokens SET last_used_at = sqlc.arg(now)::float8
WHERE workspace_id = sqlc.arg(workspace_id)::text;

-- name: ScimGetMapping :one
SELECT * FROM workspace_scim_users
WHERE workspace_id = sqlc.arg(workspace_id)::text AND user_id = sqlc.arg(user_id)::int;

-- name: ScimCountMappings :one
SELECT count(*)::bigint FROM workspace_scim_users WHERE workspace_id = sqlc.arg(workspace_id)::text;

-- name: ScimListMappingsPage :many
SELECT * FROM workspace_scim_users WHERE workspace_id = sqlc.arg(workspace_id)::text
ORDER BY user_id LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;

-- name: ScimExternalIDCollision :one
SELECT EXISTS (
  SELECT 1 FROM workspace_scim_users
  WHERE workspace_id = sqlc.arg(workspace_id)::text AND external_id = sqlc.arg(external_id)::text
    AND user_id <> sqlc.arg(user_id)::int
)::bool AS collides;

-- name: ScimUpsertMapping :one
INSERT INTO workspace_scim_users (workspace_id, user_id, external_id, display_name, active, created_at, updated_at)
VALUES (
  sqlc.arg(workspace_id)::text, sqlc.arg(user_id)::int, sqlc.arg(external_id)::text,
  sqlc.arg(display_name)::text, sqlc.arg(active)::bool, sqlc.arg(now)::float8, sqlc.arg(now)::float8
)
ON CONFLICT (workspace_id, user_id) DO UPDATE SET
  external_id = EXCLUDED.external_id, display_name = EXCLUDED.display_name,
  active = EXCLUDED.active, updated_at = EXCLUDED.updated_at
RETURNING *;

-- name: ScimUsersByIDs :many
SELECT * FROM users WHERE id = ANY(sqlc.arg(ids)::int[]) ORDER BY id;

-- name: ScimUserByLowerUsername :one
SELECT * FROM users WHERE lower(username) = sqlc.arg(username)::text ORDER BY id LIMIT 1;

-- name: ScimAddViewerMember :exec
INSERT INTO workspace_members (workspace_id, user_id, role, created_at)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(user_id)::int, 'viewer', sqlc.arg(now)::float8)
-- DO NOTHING (Python overwrites to viewer): a race with an admin adding the same member
-- must not silently demote them.
ON CONFLICT (workspace_id, user_id) DO NOTHING;

-- removeMember takes the member row lock first (the order of the RBAC routes), then
-- deletes the override rows: the opposite order deadlocked against override PUTs.
-- name: ScimLockMemberRole :one
SELECT role FROM workspace_members
WHERE workspace_id = sqlc.arg(workspace_id)::text AND user_id = sqlc.arg(user_id)::int
FOR UPDATE;

-- name: ScimCountOwnerMembers :one
SELECT count(*)::bigint FROM workspace_members
WHERE workspace_id = sqlc.arg(workspace_id)::text AND role = 'owner';

-- name: ScimWorkspaceOwnerID :one
SELECT owner_id FROM workspaces WHERE id = sqlc.arg(workspace_id)::text;

-- manager.remove_member deletes these three in order.
-- name: ScimRemoveMemberOIDC :exec
DELETE FROM workspace_oidc_identities
WHERE workspace_id = sqlc.arg(workspace_id)::text AND user_id = sqlc.arg(user_id)::int;

-- name: ScimRemoveMemberPermissions :exec
DELETE FROM workspace_member_permissions
WHERE workspace_id = sqlc.arg(workspace_id)::text AND user_id = sqlc.arg(user_id)::int;

-- name: ScimRemoveMember :exec
DELETE FROM workspace_members
WHERE workspace_id = sqlc.arg(workspace_id)::text AND user_id = sqlc.arg(user_id)::int;

-- SCIM groups: workspace_scim_groups / workspace_scim_group_members are FORCE RLS.
-- name: ScimMappingExists :one
SELECT EXISTS (
  SELECT 1 FROM workspace_scim_users
  WHERE workspace_id = sqlc.arg(workspace_id)::text AND user_id = sqlc.arg(user_id)::int
)::bool AS present;

-- name: ScimInsertGroup :exec
INSERT INTO workspace_scim_groups (id, workspace_id, external_id, display_name, created_at, updated_at)
VALUES (
  sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(external_id)::text,
  sqlc.arg(display_name)::text, sqlc.arg(now)::float8, sqlc.arg(now)::float8
);

-- name: ScimInsertGroupMember :exec
INSERT INTO workspace_scim_group_members (workspace_id, group_id, user_id, created_at)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(group_id)::text, sqlc.arg(user_id)::int, sqlc.arg(now)::float8);

-- name: ScimGetGroup :one
SELECT * FROM workspace_scim_groups
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(group_id)::text;

-- name: ScimGroupMemberIDs :many
SELECT user_id FROM workspace_scim_group_members
WHERE workspace_id = sqlc.arg(workspace_id)::text AND group_id = sqlc.arg(group_id)::text
ORDER BY user_id;

-- name: ScimGroupMembersFor :many
SELECT group_id, user_id FROM workspace_scim_group_members
WHERE workspace_id = sqlc.arg(workspace_id)::text AND group_id = ANY(sqlc.arg(group_ids)::text[])
ORDER BY group_id, user_id;

-- name: ScimCountGroups :one
SELECT count(*)::bigint FROM workspace_scim_groups
WHERE workspace_id = sqlc.arg(workspace_id)::text
  AND (sqlc.narg(display_name)::text IS NULL OR lower(display_name) = lower(sqlc.narg(display_name)::text));

-- name: ScimListGroupsPage :many
SELECT * FROM workspace_scim_groups
WHERE workspace_id = sqlc.arg(workspace_id)::text
  AND (sqlc.narg(display_name)::text IS NULL OR lower(display_name) = lower(sqlc.narg(display_name)::text))
ORDER BY display_name, id LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;

-- name: ScimUpdateGroup :exec
UPDATE workspace_scim_groups
SET display_name = sqlc.arg(display_name)::text, external_id = sqlc.arg(external_id)::text, updated_at = sqlc.arg(now)::float8
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(group_id)::text;

-- name: ScimDeleteGroupMembers :exec
DELETE FROM workspace_scim_group_members
WHERE workspace_id = sqlc.arg(workspace_id)::text AND group_id = sqlc.arg(group_id)::text;

-- name: ScimDeleteGroup :execrows
DELETE FROM workspace_scim_groups
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(group_id)::text;
