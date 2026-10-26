package identity

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// IsMember reports whether uid belongs to workspace ws or owns it
// (manager.is_member). Identity plane: no tenant GUC.
func (s *Store) IsMember(ctx context.Context, ws string, uid int32) (bool, error) {
	return s.q.IsMember(ctx, gen.IsMemberParams{WorkspaceID: ws, UserID: uid})
}

// ActiveWorkspaceID is manager.get_user_active_workspace: the stored active
// workspace while the user still belongs to it, else the first accessible one,
// else "".
func (s *Store) ActiveWorkspaceID(ctx context.Context, uid int32) (string, error) {
	return s.activeOrFirst(ctx, uid)
}

// HasPermission is manager.has_permission: the workspace owner role passes, an
// explicit per-member allow/deny wins, else the role must be in roles. The
// role is the workspace_members role, so an owner_id without a member row has
// none (as in Python's member_role).
func (s *Store) HasPermission(ctx context.Context, ws string, uid int32, permission string, roles ...string) (bool, error) {
	role, err := s.q.MemberRole(ctx, gen.MemberRoleParams{WorkspaceID: ws, UserID: uid})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if role == "owner" {
		return true, nil
	}
	if s.pool == nil {
		return false, errors.New("identity: HasPermission needs a pool-backed store")
	}
	var effect string
	var found bool
	err = tenancy.InWorkspace(ctx, s.pool, ws, func(tx pgx.Tx) error {
		rows, err := gen.New(tx).MemberPermissions(ctx, gen.MemberPermissionsParams{WorkspaceID: ws, UserID: uid})
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Permission == permission {
				effect, found = row.Effect, true
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if found {
		return effect == "allow", nil
	}
	return slices.Contains(roles, role), nil
}

// MemberRole is manager.member_role: the workspace_members role, or "" when
// the user has no member row. Identity plane: no tenant GUC.
func (s *Store) MemberRole(ctx context.Context, ws string, uid int32) (string, error) {
	role, err := s.q.MemberRole(ctx, gen.MemberRoleParams{WorkspaceID: ws, UserID: uid})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	return role, nil
}
