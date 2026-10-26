-- Governance member admin (internal/govadmin). Run inside tenancy.InWorkspace:
-- workspace_members is identity-plane, the override/OIDC/SCIM tables are FORCE RLS.

-- name: GovListMembers :many
SELECT user_id, role, created_at FROM workspace_members
WHERE workspace_id = sqlc.arg(workspace_id)::text
ORDER BY created_at, user_id;

-- name: GovUsernames :many
SELECT id, username FROM users WHERE id = ANY(sqlc.arg(ids)::int[]);

-- name: GovListOverrides :many
SELECT user_id, permission, effect FROM workspace_member_permissions
WHERE workspace_id = sqlc.arg(workspace_id)::text
ORDER BY user_id, permission;

-- Row lock serialises role change / removal / override against each other.
-- name: GovLockMemberRole :one
SELECT role FROM workspace_members
WHERE workspace_id = sqlc.arg(workspace_id)::text AND user_id = sqlc.arg(user_id)::int
FOR UPDATE;

-- name: GovUserByUsername :one
SELECT id, username FROM users WHERE username = sqlc.arg(username)::text;

-- name: GovInsertMember :execrows
INSERT INTO workspace_members (workspace_id, user_id, role, created_at)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(user_id)::int, sqlc.arg(role)::text, sqlc.arg(now)::float8)
ON CONFLICT (workspace_id, user_id) DO NOTHING;

-- name: GovUpdateMemberRole :execrows
UPDATE workspace_members SET role = sqlc.arg(role)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND user_id = sqlc.arg(user_id)::int AND role <> 'owner';

-- name: GovUpsertOverride :exec
INSERT INTO workspace_member_permissions (workspace_id, user_id, permission, effect, updated_at)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(user_id)::int, sqlc.arg(permission)::text, sqlc.arg(effect)::text, sqlc.arg(now)::float8)
ON CONFLICT (workspace_id, user_id, permission) DO UPDATE
SET effect = EXCLUDED.effect, updated_at = EXCLUDED.updated_at;

-- name: GovDeleteOverride :exec
DELETE FROM workspace_member_permissions
WHERE workspace_id = sqlc.arg(workspace_id)::text AND user_id = sqlc.arg(user_id)::int AND permission = sqlc.arg(permission)::text;

-- name: GovDeleteMemberOverrides :exec
DELETE FROM workspace_member_permissions
WHERE workspace_id = sqlc.arg(workspace_id)::text AND user_id = sqlc.arg(user_id)::int;

-- name: GovDeleteMemberOIDC :exec
DELETE FROM workspace_oidc_identities
WHERE workspace_id = sqlc.arg(workspace_id)::text AND user_id = sqlc.arg(user_id)::int;

-- Deviation D6: Python leaves SCIM group memberships dangling.
-- name: GovDeleteMemberSCIMGroups :exec
DELETE FROM workspace_scim_group_members
WHERE workspace_id = sqlc.arg(workspace_id)::text AND user_id = sqlc.arg(user_id)::int;

-- name: GovDeleteMember :exec
DELETE FROM workspace_members
WHERE workspace_id = sqlc.arg(workspace_id)::text AND user_id = sqlc.arg(user_id)::int AND role <> 'owner';
