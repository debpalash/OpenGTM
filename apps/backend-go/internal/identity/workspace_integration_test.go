//go:build integration

package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type wsEnv struct {
	pool *pgxpool.Pool
	st   *Store
}

func newWSEnv(t *testing.T, conns int32) wsEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, conns)
	return wsEnv{pool: pool, st: NewStore(pool)}
}

func (e wsEnv) user(t *testing.T, name string) User {
	t.Helper()
	seed(t, e.st, name, true, "user")
	u, err := e.st.ByUsername(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (e wsEnv) workspace(t *testing.T, id, slug string, owner *User) {
	t.Helper()
	ctx := context.Background()
	var o any
	if owner != nil {
		o = owner.ID
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1::varchar,$1::text,$2,$3)`, id, slug, o); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_ = tenancy.InWorkspace(c, e.pool, id, func(tx pgx.Tx) error {
			_, _ = tx.Exec(c, `DELETE FROM workspace_member_permissions WHERE workspace_id=$1`, id)
			_, err := tx.Exec(c, `DELETE FROM workspace_settings WHERE workspace_id=$1`, id)
			return err
		})
		_, _ = e.pool.Exec(c, `DELETE FROM user_active_workspace WHERE workspace_id=$1`, id)
		_, _ = e.pool.Exec(c, `DELETE FROM workspace_members WHERE workspace_id=$1`, id)
		_, _ = e.pool.Exec(c, `DELETE FROM workspaces WHERE id=$1`, id)
	})
}

func (e wsEnv) member(t *testing.T, ws string, u User, role string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1,$2,$3)`, ws, u.ID, role); err != nil {
		t.Fatal(err)
	}
}

func (e wsEnv) active(t *testing.T, ws string, u User) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO user_active_workspace (user_id, workspace_id) VALUES ($1,$2) ON CONFLICT (user_id) DO UPDATE SET workspace_id=EXCLUDED.workspace_id`, u.ID, ws); err != nil {
		t.Fatal(err)
	}
}

func (e wsEnv) perm(t *testing.T, ws string, u User, perm, effect string) {
	t.Helper()
	c := context.Background()
	err := tenancy.InWorkspace(c, e.pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(c, `INSERT INTO workspace_member_permissions (workspace_id,user_id,permission,effect,updated_at) VALUES ($1,$2,$3,$4,0)`, ws, u.ID, perm, effect)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e wsEnv) setting(t *testing.T, ws, key, val string) {
	t.Helper()
	c := context.Background()
	err := tenancy.InWorkspace(c, e.pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(c, `INSERT INTO workspace_settings (workspace_id,key,value) VALUES ($1,$2,$3)`, ws, key, val)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// call runs Authenticate -> Workspace -> extra -> handler as user name.
func (e wsEnv) call(name, wsHeader string, amr []string, extra ...func(http.Handler) http.Handler) (*httptest.ResponseRecorder, WorkspaceCtx) {
	var got WorkspaceCtx
	h := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = WorkspaceFrom(r.Context())
		w.WriteHeader(200)
	}))
	for i := len(extra) - 1; i >= 0; i-- {
		h = extra[i](h)
	}
	h = Workspace(e.st)(h)
	h = Authenticate(testSigner, e.st)(h)
	tok, _ := testSigner.Access(name, amr)
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if wsHeader != "" {
		req.Header.Set("X-Workspace-Id", wsHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, got
}

func wantDetail(t *testing.T, rec *httptest.ResponseRecorder, code int, detail string) {
	t.Helper()
	if rec.Code != code || rec.Body.String() != `{"detail":"`+detail+`"}` {
		t.Fatalf("got %d %s, want %d %q", rec.Code, rec.Body.String(), code, detail)
	}
}

func TestHeaderWorkspaceNotMemberIs403SameAsMissing(t *testing.T) {
	e := newWSEnv(t, 2)
	alice := e.user(t, "t5-alice")
	bob := e.user(t, "t5-bob")
	e.workspace(t, "t5-ws-a", "t5-slug-a", &alice)
	e.workspace(t, "t5-ws-b", "t5-slug-b", &bob)
	e.active(t, "t5-ws-a", alice)
	foreign, _ := e.call("t5-alice", "t5-ws-b", nil)
	missing, _ := e.call("t5-alice", "t5-ws-nope", nil)
	wantDetail(t, foreign, 403, "Workspace access denied")
	wantDetail(t, missing, 403, "Workspace access denied")
}

func TestNoWorkspaceAnywhereIs403(t *testing.T) {
	e := newWSEnv(t, 2)
	e.user(t, "t5-lonely")
	rec, _ := e.call("t5-lonely", "", nil)
	wantDetail(t, rec, 403, "No accessible workspace. Ask an admin to add you to one.")
}

func TestNoGUCSetBeforeMembershipProven(t *testing.T) {
	e := newWSEnv(t, 1)
	alice := e.user(t, "t5-alice")
	bob := e.user(t, "t5-bob")
	e.workspace(t, "t5-ws-a", "t5-slug-a", &alice)
	e.workspace(t, "t5-ws-b", "t5-slug-b", &bob)
	e.setting(t, "t5-ws-b", "k", "v")
	rec, _ := e.call("t5-alice", "t5-ws-b", nil)
	wantDetail(t, rec, 403, "Workspace access denied")
	// Same single pooled connection: the GUC must not have leaked.
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM workspace_settings`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("unscoped query saw %d tenant rows", n)
	}
	// And after a successful request too.
	if rec, _ := e.call("t5-alice", "t5-ws-a", nil); rec.Code != 200 {
		t.Fatalf("member got %d %s", rec.Code, rec.Body.String())
	}
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM workspace_settings`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after success: n=%d err=%v", n, err)
	}
}

func TestFallsBackToActiveThenFirstAccessible(t *testing.T) {
	e := newWSEnv(t, 2)
	u := e.user(t, "t5-alice")
	e.workspace(t, "t5-ws-1", "t5-slug-1", nil)
	e.workspace(t, "t5-ws-2", "t5-slug-2", nil)
	e.member(t, "t5-ws-1", u, "member")
	e.member(t, "t5-ws-2", u, "viewer")
	// No active row: first accessible (ordered by id).
	rec, c := e.call("t5-alice", "", nil)
	if rec.Code != 200 || c.WorkspaceID != "t5-ws-1" || c.Slug != "t5-slug-1" || c.Role != "member" {
		t.Fatalf("first: %d %+v", rec.Code, c)
	}
	e.active(t, "t5-ws-2", u)
	rec, c = e.call("t5-alice", "", nil)
	if rec.Code != 200 || c.WorkspaceID != "t5-ws-2" || c.Role != "viewer" {
		t.Fatalf("active: %d %+v", rec.Code, c)
	}
	// Stale active (no longer a member) falls to first accessible.
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM workspace_members WHERE workspace_id='t5-ws-2'`); err != nil {
		t.Fatal(err)
	}
	rec, c = e.call("t5-alice", "", nil)
	if rec.Code != 200 || c.WorkspaceID != "t5-ws-1" {
		t.Fatalf("stale: %d %+v", rec.Code, c)
	}
}

func TestOwnerWithoutMemberRowHasEmptyRole(t *testing.T) {
	e := newWSEnv(t, 2)
	u := e.user(t, "t5-alice")
	e.workspace(t, "t5-ws-1", "t5-slug-1", &u)
	rec, c := e.call("t5-alice", "t5-ws-1", nil)
	if rec.Code != 200 || c.Role != "" {
		t.Fatalf("%d %+v", rec.Code, c)
	}
}

func TestOwnerImpliesAnyRole(t *testing.T) {
	e := newWSEnv(t, 2)
	u := e.user(t, "t5-alice")
	e.workspace(t, "t5-ws-1", "t5-slug-1", nil)
	e.member(t, "t5-ws-1", u, "owner")
	if rec, _ := e.call("t5-alice", "t5-ws-1", nil, RequireRole("", "admin")); rec.Code != 200 {
		t.Fatalf("no-permission: %d", rec.Code)
	}
	e.perm(t, "t5-ws-1", u, "secrets.manage", "deny")
	if rec, _ := e.call("t5-alice", "t5-ws-1", nil, RequireAdmin()); rec.Code != 200 {
		t.Fatalf("owner with deny: %d", rec.Code)
	}
}

func TestPermissionDenyOverridesRole(t *testing.T) {
	e := newWSEnv(t, 2)
	u := e.user(t, "t5-alice")
	e.workspace(t, "t5-ws-1", "t5-slug-1", nil)
	e.member(t, "t5-ws-1", u, "admin")
	if rec, _ := e.call("t5-alice", "t5-ws-1", nil, RequireAdmin()); rec.Code != 200 {
		t.Fatalf("default admin: %d", rec.Code)
	}
	e.perm(t, "t5-ws-1", u, "secrets.manage", "deny")
	rec, _ := e.call("t5-alice", "t5-ws-1", nil, RequireAdmin())
	wantDetail(t, rec, 403, "Workspace permission denied: secrets.manage")
}

func TestPermissionAllowOverridesRole(t *testing.T) {
	e := newWSEnv(t, 2)
	u := e.user(t, "t5-alice")
	e.workspace(t, "t5-ws-1", "t5-slug-1", nil)
	e.member(t, "t5-ws-1", u, "viewer")
	rec, _ := e.call("t5-alice", "t5-ws-1", nil, RequireAdmin())
	wantDetail(t, rec, 403, "Workspace permission denied: secrets.manage")
	e.perm(t, "t5-ws-1", u, "secrets.manage", "allow")
	if rec, _ := e.call("t5-alice", "t5-ws-1", nil, RequireAdmin()); rec.Code != 200 {
		t.Fatalf("allow: %d", rec.Code)
	}
}

func TestViewerRejectedWithInsufficientRole(t *testing.T) {
	e := newWSEnv(t, 2)
	u := e.user(t, "t5-alice")
	e.workspace(t, "t5-ws-1", "t5-slug-1", nil)
	e.member(t, "t5-ws-1", u, "viewer")
	rec, _ := e.call("t5-alice", "t5-ws-1", nil, RequireRole("", "admin", "editor"))
	wantDetail(t, rec, 403, "Insufficient workspace role")
	e.member(t, "t5-ws-1", e.user(t, "t5-bob"), "editor")
	if rec, _ := e.call("t5-bob", "t5-ws-1", nil, RequireRole("", "admin", "editor")); rec.Code != 200 {
		t.Fatalf("editor: %d", rec.Code)
	}
}

const ssoMsg = "This workspace requires SSO. Sign in with your organization identity."

func TestEnforceSSORejectsPasswordTokenAllowsOwner(t *testing.T) {
	e := newWSEnv(t, 2)
	m := e.user(t, "t5-member")
	o := e.user(t, "t5-owner")
	e.workspace(t, "t5-ws-1", "t5-slug-1", nil)
	e.member(t, "t5-ws-1", m, "admin")
	e.member(t, "t5-ws-1", o, "owner")
	e.setting(t, "t5-ws-1", "OIDC_CONFIG", `{"enabled":true,"enforce_sso":true}`)
	wantDetail(t, mustRec(e.call("t5-member", "t5-ws-1", []string{"pwd"})), 403, ssoMsg)
	wantDetail(t, mustRec(e.call("t5-member", "t5-ws-1", nil)), 403, ssoMsg)
	if rec, _ := e.call("t5-member", "t5-ws-1", []string{"pwd", "sso"}); rec.Code != 200 {
		t.Fatalf("sso token: %d", rec.Code)
	}
	if rec, _ := e.call("t5-owner", "t5-ws-1", []string{"pwd"}); rec.Code != 200 {
		t.Fatalf("owner: %d", rec.Code)
	}
}

func mustRec(r *httptest.ResponseRecorder, _ WorkspaceCtx) *httptest.ResponseRecorder { return r }

func TestAmrAsStringAccepted(t *testing.T) {
	e := newWSEnv(t, 2)
	m := e.user(t, "t5-member")
	e.workspace(t, "t5-ws-1", "t5-slug-1", nil)
	e.member(t, "t5-ws-1", m, "member")
	e.setting(t, "t5-ws-1", "OIDC_CONFIG", `{"enforce_sso":true}`)
	// Hand-minted token with "amr":"sso" (a string, not a list).
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "t5-member", "type": "access", "amr": "sso", "exp": time.Now().Add(time.Hour).Unix()}).SignedString(testSigner.Secret)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Workspace-Id", "t5-ws-1")
	rec := httptest.NewRecorder()
	Authenticate(testSigner, e.st)(Workspace(e.st)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("string amr: %d %s", rec.Code, rec.Body.String())
	}
}

func TestOIDCConfigReadUnderRLS(t *testing.T) {
	e := newWSEnv(t, 2)
	e.workspace(t, "t5-ws-1", "t5-slug-1", nil)
	ctx := context.Background()
	enforce, err := e.st.EnforceSSO(ctx, e.pool, "t5-ws-1")
	if err != nil || enforce {
		t.Fatalf("absent: %v %v", enforce, err)
	}
	e.setting(t, "t5-ws-1", "OIDC_CONFIG", `{"enforce_sso":true,"issuer":"https://x"}`)
	if enforce, err = e.st.EnforceSSO(ctx, e.pool, "t5-ws-1"); err != nil || !enforce {
		t.Fatalf("set: %v %v", enforce, err)
	}
	// Unscoped read sees nothing: the config is only reachable inside the workspace.
	var n int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_settings`).Scan(&n)
	if n != 0 {
		t.Fatalf("unscoped saw %d", n)
	}
}

func TestRequestMetaSet(t *testing.T) {
	e := newWSEnv(t, 2)
	u := e.user(t, "t5-alice")
	e.workspace(t, "t5-ws-1", "t5-slug-1", nil)
	e.member(t, "t5-ws-1", u, "editor")
	var m Meta
	tok, _ := testSigner.Access("t5-alice", nil)
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Workspace-Id", "t5-ws-1")
	Authenticate(testSigner, e.st)(Workspace(e.st)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m = RequestMeta(r.Context())
	}))).ServeHTTP(httptest.NewRecorder(), req)
	if m.ActorUserID != u.ID || m.ActorRole != "editor" || m.WorkspaceID != "t5-ws-1" {
		t.Fatalf("%+v", m)
	}
}
