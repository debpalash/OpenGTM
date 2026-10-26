package mcp

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Capability vocabulary (services/mcp/auth.py), verbatim.
const (
	CapLeadsRead        = "leads:read"
	CapLeadsWrite       = "leads:write"
	CapSequencesEnroll  = "sequences:enroll"
	CapWorkbooksWrite   = "workbooks:write"
	CapAutomationsWrite = "automations:write"
	CapOutreachSend     = "outreach:send"
	CapLeadsDelete      = "leads:delete"
	CapWorkbooksDelete  = "workbooks:delete"
)

func capSet(c ...string) map[string]bool {
	m := make(map[string]bool, len(c))
	for _, k := range c {
		m[k] = true
	}
	return m
}

// Capability sets. Treat as read-only; Resolve hands out copies.
var (
	ReadCaps  = capSet(CapLeadsRead)
	WriteCaps = capSet(CapLeadsWrite, CapSequencesEnroll, CapWorkbooksWrite,
		CapAutomationsWrite, CapOutreachSend, CapLeadsDelete, CapWorkbooksDelete)
	// AdminOnlyCaps demand the workspace admin role (owner passes).
	AdminOnlyCaps = capSet(CapAutomationsWrite, CapOutreachSend, CapLeadsDelete, CapWorkbooksDelete)
	AllCaps       = capSet(CapLeadsRead, CapLeadsWrite, CapSequencesEnroll, CapWorkbooksWrite,
		CapAutomationsWrite, CapOutreachSend, CapLeadsDelete, CapWorkbooksDelete)
)

// Workspace roles allowed to perform a standard write / a destructive or send tool.
var (
	WriteRoles = []string{"admin", "member", "editor"}
	AdminRoles = []string{"admin"}
)

// RequireRole is the live workspace-role re-check (auth.require_role): a
// keyless context (UserID nil) passes, the owner role passes, otherwise the
// user's CURRENT workspace_members role must be one of roles.
func RequireRole(ctx context.Context, d Deps, c Ctx, roles ...string) error {
	if c.UserID == nil {
		return nil
	}
	var role string
	err := d.RunInWorkspace(ctx, c.WorkspaceID, func(tx pgx.Tx) error {
		r, e := gen.New(tx).MemberRole(ctx, gen.MemberRoleParams{WorkspaceID: c.WorkspaceID, UserID: *c.UserID})
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		role = r
		return nil
	})
	if err != nil {
		return err
	}
	if role == "owner" {
		return nil
	}
	for _, r := range roles {
		if r == role && role != "" {
			return nil
		}
	}
	return &AuthError{Msg: "insufficient workspace role"}
}
