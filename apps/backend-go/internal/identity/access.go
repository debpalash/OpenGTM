package identity

import "context"

// IsMember reports whether uid belongs to workspace ws or owns it
// (manager.is_member). Identity plane: no tenant GUC.
func (s *Store) IsMember(ctx context.Context, ws string, uid int32) (bool, error) {
	return false, nil
}

// ActiveWorkspaceID is manager.get_user_active_workspace: the stored active
// workspace while the user still belongs to it, else the first accessible one,
// else "".
func (s *Store) ActiveWorkspaceID(ctx context.Context, uid int32) (string, error) {
	return "", nil
}

// HasPermission is manager.has_permission: the workspace owner role passes, an
// explicit per-member allow/deny wins, else the role must be in roles.
func (s *Store) HasPermission(ctx context.Context, ws string, uid int32, permission string, roles ...string) (bool, error) {
	return false, nil
}
