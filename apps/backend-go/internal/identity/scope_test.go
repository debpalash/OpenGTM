package identity

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
)

// runClaims runs mw with a principal whose token is scoped to ws ("" = unscoped).
func runClaims(mw func(http.Handler) http.Handler, u User, ws string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/x", nil)
	req = req.WithContext(WithPrincipal(req.Context(), Principal{User: u, Claims: auth.Claims{Sub: u.Username, Workspace: ws}}))
	rec := httptest.NewRecorder()
	mw(ok200()).ServeHTTP(rec, req)
	return rec
}

// A workspace-scoped (SSO) session is never a global admin session.
func TestScopedSessionIsNotGlobalAdmin(t *testing.T) {
	rec := runClaims(RequireGlobalAdmin(), User{Username: "root", IsAdmin: true}, "ws-1")
	if rec.Code != 403 || rec.Body.String() != `{"detail":"Not authorized"}` {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if rec := runClaims(RequireGlobalAdmin(), User{Username: "root", IsAdmin: true}, ""); rec.Code != 200 {
		t.Fatalf("unscoped admin got %d", rec.Code)
	}
}

// Query tokens (SSE / WebSocket) do not name the workspace up front: a scoped token is refused by the
// workspace-less entry point and accepted by the workspace-bound one only for its own workspace.
func TestQueryTokenScope(t *testing.T) {
	tok, _ := testSigner.AccessFor("alice", []string{"sso"}, "ws-1")
	_, err := AuthenticateQueryToken(testSigner, nil, tok, false)
	if e, _ := err.(*QueryTokenError); e == nil || e.Status != 401 || e.Detail != "Invalid token" {
		t.Fatalf("unbound: %v", err)
	}
	_, err = AuthenticateQueryTokenForWorkspace(t.Context(), testSigner, nil, tok, false, "ws-2")
	if e, _ := err.(*QueryTokenError); e == nil || e.Status != 403 || e.Detail != "Workspace access denied" {
		t.Fatalf("other workspace: %v", err)
	}
	_, err = AuthenticateQueryTokenForWorkspace(t.Context(), testSigner, nil, tok, false, "")
	if e, _ := err.(*QueryTokenError); e == nil || e.Status != 403 {
		t.Fatalf("no workspace: %v", err)
	}
}
