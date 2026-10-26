package govadmin

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

// Python gates every member and override write on governance.manage alone, so a delegated manager (an editor
// holding a governance.manage allow override) can promote themselves to admin. These checks close that path
// without changing anything an admin or owner can do: nobody may grant a role, or manage a member, above their own
// role, and nobody may allow a permission they do not hold themselves. They run in the caller's transaction, after
// locking the actor's and target's member rows, so a concurrent demotion of the actor cannot slip between the
// check and the write.
var (
	ErrForbidden  = errors.New("Workspace permission denied: governance.manage") // 403 (decided in-tx, as the route gate)
	ErrEscalation = errors.New("Cannot grant or manage access above your own")   // 403
)

var roleRank = map[string]int{"viewer": 1, "member": 2, "editor": 3, "admin": 4, "owner": 5}

func rank(role string) int { return roleRank[role] }

// Authorize locks the actor's and target's member rows (ascending user id, so two concurrent writers cannot
// deadlock) and re-checks that the actor still holds governance.manage through identity.HasPermissionTx, the single
// decision function behind the route gate. It returns the actor's role and the target's role ("" when the target has
// no member row). tx must be inside tenancy.InWorkspace(ws).
func Authorize(ctx context.Context, tx pgx.Tx, ws string, actor, target int32) (actorRole, targetRole string, err error) {
	ids := []int32{actor}
	if target != actor {
		ids = append(ids, target)
	}
	rows, err := tx.Query(ctx, `SELECT user_id, role FROM workspace_members
		WHERE workspace_id = $1 AND user_id = ANY($2::int[]) ORDER BY user_id FOR UPDATE`, ws, ids)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	roles := map[int32]string{}
	for rows.Next() {
		var id int32
		var role string
		if err := rows.Scan(&id, &role); err != nil {
			return "", "", err
		}
		roles[id] = role
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	rows.Close()
	ok, err := identity.HasPermissionTx(ctx, tx, ws, actor, "governance.manage", RolesFor("governance.manage")...)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", ErrForbidden
	}
	return roles[actor], roles[target], nil
}

func canManage(actorRole, targetRole string) bool {
	// Owner targets are immutable and answered 409 by the domain functions; do not mask that with a 403.
	return targetRole == "" || targetRole == "owner" || rank(targetRole) <= rank(actorRole)
}

// CheckAddMember authorizes POST /members: the granted role may not exceed the actor's.
func CheckAddMember(ctx context.Context, tx pgx.Tx, ws string, actor int32, role string) error {
	actorRole, _, err := Authorize(ctx, tx, ws, actor, actor)
	if err != nil {
		return err
	}
	if validRole(role) && rank(role) > rank(actorRole) {
		return ErrEscalation
	}
	return nil
}

// CheckSetRole authorizes PATCH /members/{id}: neither the current nor the new role may exceed the actor's.
func CheckSetRole(ctx context.Context, tx pgx.Tx, ws string, actor, target int32, role string) error {
	actorRole, targetRole, err := Authorize(ctx, tx, ws, actor, target)
	if err != nil {
		return err
	}
	if !canManage(actorRole, targetRole) || (validRole(role) && targetRole != "" && targetRole != "owner" && rank(role) > rank(actorRole)) {
		return ErrEscalation
	}
	return nil
}

// CheckRemove authorizes DELETE /members/{id}.
func CheckRemove(ctx context.Context, tx pgx.Tx, ws string, actor, target int32) error {
	actorRole, targetRole, err := Authorize(ctx, tx, ws, actor, target)
	if err != nil {
		return err
	}
	if !canManage(actorRole, targetRole) {
		return ErrEscalation
	}
	return nil
}

// CheckSetOverride authorizes PUT /rbac/{id}/{permission}: the target may not outrank the actor, and an allow
// needs the actor to hold the permission. Unknown permissions and missing or owner targets fall through so the
// domain function answers with Python's 404 / 409 in Python's order.
func CheckSetOverride(ctx context.Context, tx pgx.Tx, ws string, actor, target int32, permission string, effect *string) error {
	actorRole, targetRole, err := Authorize(ctx, tx, ws, actor, target)
	if err != nil {
		return err
	}
	if _, known := lookup(permission); !known || targetRole == "" || targetRole == "owner" {
		return nil
	}
	if !canManage(actorRole, targetRole) {
		return ErrEscalation
	}
	if effect != nil && *effect == "allow" {
		held, err := identity.HasPermissionTx(ctx, tx, ws, actor, permission, RolesFor(permission)...)
		if err != nil {
			return err
		}
		if !held {
			return ErrEscalation
		}
	}
	return nil
}
