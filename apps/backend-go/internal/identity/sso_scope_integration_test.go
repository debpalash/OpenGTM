//go:build integration

package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// serve runs the request through mws (outermost first) with tok as bearer and returns the
// recorder, the resolved workspace (if Workspace ran) and the X-Workspace-Id the handler saw.
func serveScoped(t *testing.T, pattern, target, tok, wsHeader string, mws ...func(http.Handler) http.Handler) (*httptest.ResponseRecorder, WorkspaceCtx, string) {
	t.Helper()
	var got WorkspaceCtx
	var seen string
	h := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = WorkspaceFrom(r.Context())
		seen = r.Header.Get("X-Workspace-Id")
		w.WriteHeader(200)
	}))
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	mux := http.NewServeMux()
	mux.Handle(pattern, h)
	req := httptest.NewRequest("GET", target, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if wsHeader != "" {
		req.Header.Set("X-Workspace-Id", wsHeader)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec, got, seen
}

// An SSO session (sso_ws claim) is valid for its workspace only; password sessions are unchanged.
func TestSSOScopedSessionStaysInItsWorkspace(t *testing.T) {
	e := newWSEnv(t, 4)
	alice := e.user(t, "t7s-alice")
	e.workspace(t, "t7s-ws-a", "t7s-slug-a", nil)
	e.workspace(t, "t7s-ws-b", "t7s-slug-b", nil)
	e.member(t, "t7s-ws-a", alice, "member")
	e.member(t, "t7s-ws-b", alice, "member")
	e.active(t, "t7s-ws-b", alice)

	sso, _ := testSigner.AccessFor("t7s-alice", []string{"sso"}, "t7s-ws-a")
	pwd, _ := testSigner.Access("t7s-alice", []string{"pwd"})
	authn, ws := Authenticate(testSigner, e.st), Workspace(e.st)

	// Another workspace by header: 403 even though alice is a member there.
	rec, _, _ := serveScoped(t, "GET /x", "/x", sso, "t7s-ws-b", authn, ws)
	wantDetail(t, rec, 403, "Workspace access denied")
	// No header: the scope wins over the stored active workspace (B).
	rec, got, _ := serveScoped(t, "GET /x", "/x", sso, "", authn, ws)
	if rec.Code != 200 || got.WorkspaceID != "t7s-ws-a" {
		t.Fatalf("no header: %d %+v", rec.Code, got)
	}
	rec, got, _ = serveScoped(t, "GET /x", "/x", sso, "t7s-ws-a", authn, ws)
	if rec.Code != 200 || got.WorkspaceID != "t7s-ws-a" || got.Claims.Workspace != "t7s-ws-a" {
		t.Fatalf("own workspace: %d %+v", rec.Code, got)
	}
	// Password tokens are unaffected.
	if rec, got, _ := serveScoped(t, "GET /x", "/x", pwd, "t7s-ws-b", authn, ws); rec.Code != 200 || got.WorkspaceID != "t7s-ws-b" {
		t.Fatalf("password other ws: %d %+v", rec.Code, got)
	}
	if rec, got, _ := serveScoped(t, "GET /x", "/x", pwd, "", authn, ws); rec.Code != 200 || got.WorkspaceID != "t7s-ws-b" {
		t.Fatalf("password active ws: %d %+v", rec.Code, got)
	}

	// Routes behind Authenticate only (no Workspace middleware): a foreign header or {ws_id} is refused,
	// and an absent header is pinned to the scope for handlers that read it.
	rec, _, _ = serveScoped(t, "GET /x", "/x", sso, "t7s-ws-b", authn)
	wantDetail(t, rec, 403, "Workspace access denied")
	if rec, _, seen := serveScoped(t, "GET /x", "/x", sso, "", authn); rec.Code != 200 || seen != "t7s-ws-a" {
		t.Fatalf("pinned header: %d %q", rec.Code, seen)
	}
	rec, _, _ = serveScoped(t, "GET /api/workspaces/{ws_id}", "/api/workspaces/t7s-ws-b", sso, "", authn)
	wantDetail(t, rec, 403, "Workspace access denied")
	if rec, _, _ := serveScoped(t, "GET /api/workspaces/{ws_id}", "/api/workspaces/t7s-ws-a", sso, "", authn); rec.Code != 200 {
		t.Fatalf("own {ws_id}: %d", rec.Code)
	}
	if rec, _, seen := serveScoped(t, "GET /api/workspaces/{ws_id}", "/api/workspaces/t7s-ws-b", pwd, "", authn); rec.Code != 200 || seen != "" {
		t.Fatalf("password {ws_id}: %d %q", rec.Code, seen)
	}

	// A scoped session loses access once the member row in its workspace is gone.
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM workspace_members WHERE workspace_id='t7s-ws-a' AND user_id=$1`, alice.ID); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = serveScoped(t, "GET /x", "/x", sso, "", authn, ws)
	wantDetail(t, rec, 403, "Workspace access denied")
}

// The workspace-bound query-token entry point (SSE / WebSocket) honours the scope.
func TestQueryTokenForWorkspaceScope(t *testing.T) {
	e := newWSEnv(t, 2)
	e.user(t, "t7s-bob")
	ctx := context.Background()
	sso, _ := testSigner.AccessFor("t7s-bob", []string{"sso"}, "t7s-ws-q")
	if p, err := AuthenticateQueryTokenForWorkspace(ctx, testSigner, e.st, sso, false, "t7s-ws-q"); err != nil || p.Claims.Workspace != "t7s-ws-q" {
		t.Fatalf("own: %+v %v", p, err)
	}
	pwd, _ := testSigner.Access("t7s-bob", []string{"pwd"})
	if _, err := AuthenticateQueryTokenForWorkspace(ctx, testSigner, e.st, pwd, false, "anything"); err != nil {
		t.Fatalf("password: %v", err)
	}
	if _, err := AuthenticateQueryTokenCtx(ctx, testSigner, e.st, pwd, false); err != nil {
		t.Fatalf("password unbound: %v", err)
	}
}
