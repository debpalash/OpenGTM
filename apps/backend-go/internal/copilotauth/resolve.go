// Package copilotauth is _resolve_chat_workspace: the single tenant decision
// point of the chat routes. It is deliberately not identity.Workspace (different
// status codes for inactive users, no SSO enforcement, self-host mode).
package copilotauth

import (
	"errors"
	"net/http"

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
	SelfHost    bool
}

// ToolUserID is the user id to send with every Python tool call.
func (r Resolved) ToolUserID() (*int32, error) { return nil, errors.New("not implemented") }

// Resolve authorizes the request's workspace.
func Resolve(r *http.Request, d Deps) (Resolved, *Err) {
	return Resolved{}, &Err{Status: http.StatusNotImplemented, Detail: "not implemented"}
}
