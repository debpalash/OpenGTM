package govidp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// SSO identity linking for the /auth/sso callback (S-M5). Python linked an unknown
// (issuer, subject) to any deployment user whose username equalled the IdP email, including
// owners and admins of other workspaces. Here an identity resolves only:
//   - through an existing (workspace, issuer, subject) binding, or
//   - to an existing member of THIS workspace whose username is the verified email and who is
//     not an owner or admin here, not a global admin, owns no workspace and is a member of no
//     other workspace (the session is not workspace-scoped, so this workspace's IdP must not be
//     able to mint a login for an account that matters elsewhere), or
//   - with auto_provision, to a brand-new user (no user of that name exists anywhere) added to
//     this workspace with the configured default role.
// A user of that name who is not a member of this workspace is never linked or added.

// SSO callback refusals (shown to the user in the login fragment).
const (
	MsgSSONoAccount     = "No provisioned OpenGTM account matches this identity"
	MsgSSONotMember     = "This account is not provisioned in the workspace"
	MsgSSOPrivileged    = "SSO cannot be linked automatically to an owner or admin account"
	MsgSSOCrossWS       = "SSO cannot be linked automatically to an account that belongs to other workspaces"
	MsgSSOOtherIdentity = "This account is already linked to another SSO identity"
	MsgSSOInactive      = "This SSO identity is not an active workspace member"
)

// ResolveSSOUser returns the user id the verified identity signs in as, creating the binding
// (and, with auto_provision, the user and membership) when allowed. hashPassword makes the
// unusable password of a provisioned user. tx must be inside tenancy.InWorkspace(ws).
func ResolveSSOUser(ctx context.Context, tx pgx.Tx, ws, issuer, subject, email string, cfg OIDCConfig,
	hashPassword func() (string, error)) (int32, error) {
	// Serialize callbacks for one identity so two concurrent first logins converge.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('oidc-link:' || $1::text || chr(10) || $2::text || chr(10) || $3::text, 0))`,
		ws, issuer, subject); err != nil {
		return 0, fmt.Errorf("govidp: sso lock: %w", err)
	}
	var uid int32
	err := tx.QueryRow(ctx, `SELECT user_id FROM workspace_oidc_identities WHERE workspace_id = $1 AND issuer = $2 AND subject = $3`,
		ws, issuer, subject).Scan(&uid)
	switch {
	case err == nil:
		return uid, checkActiveMember(ctx, tx, ws, uid)
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, fmt.Errorf("govidp: sso binding: %w", err)
	}

	var memberRole string
	var globalAdmin bool
	err = tx.QueryRow(ctx, `SELECT u.id, m.role, COALESCE(u.is_admin, false) OR COALESCE(u.role, '') = 'admin'
		FROM users u JOIN workspace_members m ON m.user_id = u.id AND m.workspace_id = $1
		WHERE u.username = $2 FOR UPDATE OF m`, ws, email).Scan(&uid, &memberRole, &globalAdmin)
	switch {
	case err == nil:
		var owner bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspaces WHERE id = $1 AND owner_id = $2)`, ws, uid).Scan(&owner); err != nil {
			return 0, fmt.Errorf("govidp: sso owner: %w", err)
		}
		if owner || globalAdmin || memberRole == "owner" || memberRole == "admin" {
			return 0, &ValueError{MsgSSOPrivileged}
		}
		var elsewhere bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspace_members WHERE user_id = $1 AND workspace_id <> $2)
			OR EXISTS (SELECT 1 FROM workspaces WHERE owner_id = $1)`, uid, ws).Scan(&elsewhere); err != nil {
			return 0, fmt.Errorf("govidp: sso other workspaces: %w", err)
		}
		if elsewhere {
			return 0, &ValueError{MsgSSOCrossWS}
		}
	case errors.Is(err, pgx.ErrNoRows):
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE username = $1)`, email).Scan(&exists); err != nil {
			return 0, fmt.Errorf("govidp: sso user: %w", err)
		}
		if exists {
			return 0, &ValueError{MsgSSONotMember}
		}
		if !cfg.AutoProvision {
			return 0, &ValueError{MsgSSONoAccount}
		}
		switch cfg.DefaultRole {
		case "viewer", "member", "editor":
		default:
			return 0, &ValueError{MsgSSONoAccount}
		}
		hash, err := hashPassword()
		if err != nil {
			return 0, err
		}
		// A concurrent insert of the same name loses on the unique username and is refused.
		err = tx.QueryRow(ctx, `INSERT INTO users (username, hashed_password, is_active, is_admin, role, created_at)
			VALUES ($1, $2, true, false, 'user', now() AT TIME ZONE 'UTC') ON CONFLICT DO NOTHING RETURNING id`, email, hash).Scan(&uid)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, &ValueError{MsgSSONotMember}
		}
		if err != nil {
			return 0, fmt.Errorf("govidp: sso provision user: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_members (workspace_id, user_id, role, created_at) VALUES ($1, $2, $3, $4)`,
			ws, uid, cfg.DefaultRole, float64(time.Now().UnixNano())/1e9); err != nil {
			return 0, fmt.Errorf("govidp: sso provision member: %w", err)
		}
	default:
		return 0, fmt.Errorf("govidp: sso member lookup: %w", err)
	}

	// One binding per (workspace, user): a member already linked to another subject is refused.
	var other bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspace_oidc_identities WHERE workspace_id = $1 AND user_id = $2)`,
		ws, uid).Scan(&other); err != nil {
		return 0, fmt.Errorf("govidp: sso binding by user: %w", err)
	}
	if other {
		return 0, &ValueError{MsgSSOOtherIdentity}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workspace_oidc_identities (workspace_id, issuer, subject, user_id, email, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, ws, issuer, subject, uid, email, float64(time.Now().UnixNano())/1e9); err != nil {
		return 0, fmt.Errorf("govidp: sso bind: %w", err)
	}
	return uid, checkActiveMember(ctx, tx, ws, uid)
}

func checkActiveMember(ctx context.Context, tx pgx.Tx, ws string, uid int32) error {
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users u JOIN workspace_members m ON m.user_id = u.id AND m.workspace_id = $1
		WHERE u.id = $2 AND COALESCE(u.is_active, false))`, ws, uid).Scan(&ok); err != nil {
		return fmt.Errorf("govidp: sso member check: %w", err)
	}
	if !ok {
		return &ValueError{MsgSSOInactive}
	}
	return nil
}

// NormalizeEmail is Python's str(email).strip().lower() for the IdP email claim.
func NormalizeEmail(s string) string { return strings.ToLower(pyStrip(s)) }
