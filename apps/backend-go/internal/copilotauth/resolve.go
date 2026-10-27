// Package copilotauth is _resolve_chat_workspace: the single tenant decision
// point of the chat routes. It is deliberately not identity.Workspace (different
// status codes for inactive users, no SSO enforcement, self-host mode).
package copilotauth

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

// Err is an HTTP refusal (FastAPI HTTPException shape).
type Err struct {
	Status  int
	Detail  string
	Headers []httpx.Header
}

func (e *Err) Error() string { return e.Detail }

// Write answers the refusal as {"detail": ...}.
func (e *Err) Write(w http.ResponseWriter) { httpx.Detail(w, e.Status, e.Detail, e.Headers...) }

// Deps are the collaborators of Resolve.
type Deps struct {
	Signer      auth.Signer
	Identity    *identity.Store
	DB          gen.DBTX
	RequireAuth bool
}

// Resolved is the authorized chat tenant.
type Resolved struct {
	WorkspaceID string
	Slug        string
	UserID      *int32
	selfHost    bool // set only by Resolve (keyless branch)
}

// IsSelfHost reports a keyless (self-host) resolution; only Resolve can make one.
func (r Resolved) IsSelfHost() bool { return r.selfHost }

// ToolUserID is the user id every Python tool call must carry. The Python
// /internal/copilot/tool endpoint skips the dangerous-tool role check for a
// null user_id, so a nil id is only legal for a self-host Resolved built by
// Resolve; a cloud or zero-valued Resolved without a user fails closed.
func (r Resolved) ToolUserID() (*int32, error) {
	if r.UserID != nil {
		return r.UserID, nil
	}
	if r.selfHost && r.WorkspaceID != "" {
		return nil, nil
	}
	return nil, errors.New("copilotauth: no authenticated user for tool call")
}

func unauthorized() *Err {
	return &Err{Status: http.StatusUnauthorized, Detail: "Could not validate credentials",
		Headers: []httpx.Header{{Key: "WWW-Authenticate", Value: "Bearer"}}}
}

func internal() *Err {
	return &Err{Status: http.StatusInternalServerError, Detail: "Internal Server Error"}
}

// Resolve authorizes the request's workspace. SSO enforcement is not applied
// (Python's chat resolver does not either; parity).
func Resolve(r *http.Request, d Deps) (Resolved, *Err) {
	ctx := r.Context()
	q := gen.New(d.DB)

	if !d.RequireAuth {
		ws, err := q.GetAppSetting(ctx, "ACTIVE_WORKSPACE")
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Resolved{}, internal()
		}
		if ws == "" {
			ws, err = q.McpAuthMainWorkspaceID(ctx)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return Resolved{}, internal()
			}
		}
		if ws == "" {
			return Resolved{}, &Err{Status: http.StatusForbidden, Detail: noWorkspace}
		}
		slug, err := q.McpAuthWorkspaceSlug(ctx, ws)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Resolved{}, internal()
		}
		if slug == "" {
			slug = "main"
		}
		return Resolved{WorkspaceID: ws, Slug: slug, selfHost: true}, nil
	}

	tok, ok := identity.BearerToken(r)
	if !ok {
		return Resolved{}, unauthorized()
	}
	claims, err := d.Signer.ParseAccess(tok) // rejects refresh tokens and empty sub
	if err != nil {
		return Resolved{}, unauthorized()
	}
	u, err := d.Identity.ByUsername(ctx, claims.Sub)
	if errors.Is(err, identity.ErrNotFound) || (err == nil && !u.IsActive) {
		return Resolved{}, unauthorized()
	}
	if err != nil {
		return Resolved{}, internal()
	}

	// An SSO session (sso_ws) acts in its own workspace only: the scope replaces the stored active
	// workspace and any other workspace is refused, as identity.Workspace does.
	ws := r.Header.Get("X-Workspace-Id")
	if ws == "" {
		ws = claims.Workspace
	}
	if ws == "" {
		if ws, err = d.Identity.ActiveWorkspaceID(ctx, u.ID); err != nil {
			return Resolved{}, internal()
		}
	}
	if ws == "" {
		return Resolved{}, &Err{Status: http.StatusForbidden, Detail: noWorkspace}
	}
	if !claims.AllowsWorkspace(ws) {
		return Resolved{}, &Err{Status: http.StatusForbidden, Detail: "Workspace access denied"}
	}
	member, err := d.Identity.IsMember(ctx, ws, u.ID)
	if err != nil {
		return Resolved{}, internal()
	}
	if !member {
		// Same answer for a missing and a foreign workspace.
		return Resolved{}, &Err{Status: http.StatusForbidden, Detail: "Workspace access denied"}
	}
	slug, err := q.McpAuthWorkspaceSlug(ctx, ws)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && slug == "") {
		return Resolved{}, &Err{Status: http.StatusNotFound, Detail: "Workspace not found"}
	}
	if err != nil {
		return Resolved{}, internal()
	}
	uid := u.ID
	return Resolved{WorkspaceID: ws, Slug: slug, UserID: &uid}, nil
}

const noWorkspace = "No accessible workspace. Ask an admin to add you to one."
