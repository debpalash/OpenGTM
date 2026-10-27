//go:build integration

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ratelimit"
)

const goodPW = "correct-horse-battery"

// setupEnv is a router over private, empty identity tables (setup needs an empty users table); see
// newIsolatedIdentityPool. The shared tables are never touched.
type setupEnv struct {
	h    http.Handler
	pool *pgxpool.Pool
	st   *identity.Store
}

func newSetupEnv(t *testing.T, token string, lim *ratelimit.Limiter) setupEnv {
	t.Helper()
	pool := newIsolatedIdentityPool(t, 16)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	RegisterAuth(r, Deps{Pool: pool, Signer: testSigner, Identity: st, SetupToken: token, Limiter: lim})
	return setupEnv{h: r.Handler(), pool: pool, st: st}
}

func (e setupEnv) post(body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/auth/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e setupEnv) status(t *testing.T) (required, tokenRequired bool, rec *httptest.ResponseRecorder) {
	t.Helper()
	rec = httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("GET", "/auth/setup/status", nil))
	var b struct {
		SetupRequired bool `json:"setup_required"`
		TokenRequired bool `json:"token_required"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &b) != nil {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}
	return b.SetupRequired, b.TokenRequired, rec
}

func setupBody(user, pw, extra string) string {
	b := `{"username":"` + user + `","password":"` + pw + `"`
	if extra != "" {
		b += "," + extra
	}
	return b + "}"
}

func count(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSetupStatusReportsRequiredAndTokenFlag(t *testing.T) {
	e := newSetupEnv(t, "", nil)
	req, tok, rec := e.status(t)
	if !req || tok {
		t.Fatalf("empty db, no token: required=%v token=%v", req, tok)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	r := httpx.NewRouter()
	RegisterAuth(r, Deps{Pool: e.pool, Signer: testSigner, Identity: e.st, SetupToken: "tok"})
	e2 := setupEnv{h: r.Handler(), pool: e.pool, st: e.st}
	if req, tok, _ = e2.status(t); !req || !tok {
		t.Fatalf("token configured: required=%v token=%v", req, tok)
	}
}

func TestSetupHappyPath(t *testing.T) {
	e := newSetupEnv(t, "", nil)
	rec := e.post(setupBody("  Boss  ", goodPW, `"workspace_name":"Acme Co"`))
	if rec.Code != 200 {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	var tr map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &tr); err != nil || tr["access_token"] == "" || tr["refresh_token"] == "" || tr["token_type"] != "bearer" {
		t.Fatalf("token body: %s", rec.Body)
	}
	// The returned access token authenticates.
	req := httptest.NewRequest("GET", "/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+tr["access_token"])
	me := httptest.NewRecorder()
	e.h.ServeHTTP(me, req)
	var u struct {
		ID       int32  `json:"id"`
		Username string `json:"username"`
		IsAdmin  bool   `json:"is_admin"`
		Role     string `json:"role"`
		Active   bool   `json:"is_active"`
	}
	if me.Code != 200 || json.Unmarshal(me.Body.Bytes(), &u) != nil {
		t.Fatalf("me: %d %s", me.Code, me.Body)
	}
	if u.Username != "Boss" || !u.IsAdmin || u.Role != "admin" || !u.Active {
		t.Fatalf("user = %+v", u)
	}
	// Password stored as bcrypt and usable on /auth/token.
	_, hash, err := e.st.PasswordHashByUsername(context.Background(), "Boss")
	if err != nil || !strings.Contains(hash[:7], "$12$") || !auth.VerifyPassword(goodPW, hash) {
		t.Fatalf("stored hash prefix/verify failed (err=%v)", err)
	}
	// Workspace: one, slug main, owned, member and active.
	var wsID, name string
	var owner int32
	if err := e.pool.QueryRow(context.Background(), `SELECT id, name, owner_id FROM workspaces WHERE slug='main'`).Scan(&wsID, &name, &owner); err != nil {
		t.Fatal(err)
	}
	if name != "Acme Co" || owner != u.ID {
		t.Fatalf("workspace name=%q owner=%d", name, owner)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM workspaces`); n != 1 {
		t.Fatalf("workspaces = %d", n)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM workspace_members WHERE workspace_id=$1 AND user_id=$2 AND role='owner'`, wsID, u.ID); n != 1 {
		t.Fatalf("owner membership = %d", n)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM user_active_workspace WHERE workspace_id=$1 AND user_id=$2`, wsID, u.ID); n != 1 {
		t.Fatalf("active workspace = %d", n)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM app_settings WHERE key='setup_complete'`); n != 1 {
		t.Fatalf("marker = %d", n)
	}
	if req, _, _ := e.status(t); req {
		t.Fatal("status still setup_required after setup")
	}
}

func TestSetupDefaultWorkspaceNameIsMain(t *testing.T) {
	e := newSetupEnv(t, "", nil)
	if rec := e.post(setupBody("boss", goodPW, "")); rec.Code != 200 {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body)
	}
	var name string
	if err := e.pool.QueryRow(context.Background(), `SELECT name FROM workspaces WHERE slug='main'`).Scan(&name); err != nil || name != "Main" {
		t.Fatalf("name=%q err=%v", name, err)
	}
}

func TestSetupSecondCallIs409(t *testing.T) {
	e := newSetupEnv(t, "", nil)
	if rec := e.post(setupBody("boss", goodPW, "")); rec.Code != 200 {
		t.Fatalf("first: %d", rec.Code)
	}
	rec := e.post(setupBody("other", goodPW, ""))
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "Setup already completed") {
		t.Fatalf("second: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM users`); n != 1 {
		t.Fatalf("users = %d", n)
	}
}

func TestSetupIs409WhenAUserAlreadyExists(t *testing.T) {
	e := newSetupEnv(t, "", nil)
	if _, err := e.st.Insert(context.Background(), identity.NewUser{Username: "seeded", HashedPassword: "x", IsActive: true, Role: "user"}); err != nil {
		t.Fatal(err)
	}
	if rec := e.post(setupBody("boss", goodPW, "")); rec.Code != 409 {
		t.Fatalf("status %d", rec.Code)
	}
	if req, _, _ := e.status(t); req {
		t.Fatal("setup_required with an existing user")
	}
}

func TestSetupMarkerBlocksEvenWithZeroUsers(t *testing.T) {
	e := newSetupEnv(t, "", nil)
	if rec := e.post(setupBody("boss", goodPW, "")); rec.Code != 200 {
		t.Fatalf("first: %d", rec.Code)
	}
	u, _ := e.st.ByUsername(context.Background(), "boss")
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM user_active_workspace; DELETE FROM workspace_members; DELETE FROM workspaces`); err != nil {
		t.Fatal(err)
	}
	if err := e.st.Delete(context.Background(), u.ID); err != nil {
		t.Fatal(err)
	}
	if req, _, _ := e.status(t); req {
		t.Fatal("status re-opened setup after all users were deleted")
	}
	if rec := e.post(setupBody("again", goodPW, "")); rec.Code != 409 {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestSetupConcurrentExactlyOneWins(t *testing.T) {
	e := newSetupEnv(t, "", nil)
	const n = 10
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			names := []string{"alice", "bob", "carol", "dave", "erin", "frank", "grace", "heidi", "ivan", "judy"}
			codes[i] = e.post(setupBody(names[i], goodPW, "")).Code
		}(i)
	}
	close(start)
	wg.Wait()
	ok, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case 200:
			ok++
		case 409:
			conflict++
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Fatalf("codes = %v", codes)
	}
	if c := count(t, e.pool, `SELECT count(*) FROM users`); c != 1 {
		t.Fatalf("users = %d", c)
	}
	if c := count(t, e.pool, `SELECT count(*) FROM workspaces`); c != 1 {
		t.Fatalf("workspaces = %d", c)
	}
}

func TestSetupTokenEnforced(t *testing.T) {
	e := newSetupEnv(t, "s3cret-token", nil)
	if rec := e.post(setupBody("boss", goodPW, "")); rec.Code != 401 {
		t.Fatalf("missing token: %d %s", rec.Code, rec.Body)
	}
	if rec := e.post(setupBody("boss", goodPW, `"setup_token":"nope"`)); rec.Code != 403 {
		t.Fatalf("bad token: %d %s", rec.Code, rec.Body)
	}
	if rec := e.post(setupBody("boss", goodPW, `"setup_token":"s3cret-token-extra"`)); rec.Code != 403 {
		t.Fatalf("prefix-extended token: %d", rec.Code)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM users`); n != 0 {
		t.Fatalf("users created by rejected calls: %d", n)
	}
	if rec := e.post(setupBody("boss", goodPW, `"setup_token":"s3cret-token"`)); rec.Code != 200 {
		t.Fatalf("good token: %d %s", rec.Code, rec.Body)
	}
}

func TestSetupPasswordAndUsernamePolicy(t *testing.T) {
	e := newSetupEnv(t, "", nil)
	cases := map[string]string{
		"short password":    setupBody("boss", "short-pw", ""),
		"too long (73 B)":   setupBody("boss", strings.Repeat("a", 73), ""),
		"equals username":   setupBody("administrator", "administrator", ""),
		"common password":   setupBody("boss", "password1234", ""),
		"short username":    setupBody("ab", goodPW, ""),
		"long username":     setupBody(strings.Repeat("u", 65), goodPW, ""),
		"blank username":    setupBody("   ", goodPW, ""),
		"missing password":  `{"username":"boss"}`,
		"password not text": `{"username":"boss","password":12345678901234}`,
	}
	for name, body := range cases {
		rec := e.post(body)
		if rec.Code != 422 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
			continue
		}
		var b struct {
			Detail []struct {
				Loc []any `json:"loc"`
			} `json:"detail"`
		}
		if json.Unmarshal(rec.Body.Bytes(), &b) != nil || len(b.Detail) == 0 || b.Detail[0].Loc[0] != "body" {
			t.Errorf("%s: not a FastAPI 422 body: %s", name, rec.Body)
		}
		if strings.Contains(rec.Body.String(), goodPW) {
			t.Errorf("%s: response echoes the password", name)
		}
	}
	if n := count(t, e.pool, `SELECT count(*) FROM users`); n != 0 {
		t.Fatalf("users after rejected calls: %d", n)
	}
	// Exactly 72 bytes is the bcrypt limit and is accepted.
	if rec := e.post(setupBody("boss", strings.Repeat("x7Q", 24), "")); rec.Code != 200 {
		t.Fatalf("72-byte password: %d %s", rec.Code, rec.Body)
	}
}

func TestSetupBackfillsExistingMainWorkspace(t *testing.T) {
	e := newSetupEnv(t, "", nil)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO workspaces (id, name, slug, owner_id, created_at, updated_at) VALUES ('legacy-main','Legacy','main',NULL,1,1)`,
		`INSERT INTO workspaces (id, name, slug, owner_id, created_at, updated_at) VALUES ('legacy-other','Other','other',NULL,1,1)`,
	} {
		if _, err := e.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if rec := e.post(setupBody("boss", goodPW, `"workspace_name":"Ignored"`)); rec.Code != 200 {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM workspaces`); n != 2 {
		t.Fatalf("workspaces = %d (a new one was created)", n)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM workspaces w JOIN users u ON u.id = w.owner_id WHERE u.username='boss'`); n != 2 {
		t.Fatalf("owned workspaces = %d", n)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM workspace_members WHERE role='owner'`); n != 2 {
		t.Fatalf("owner memberships = %d", n)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM user_active_workspace WHERE workspace_id='legacy-main'`); n != 1 {
		t.Fatalf("active workspace not main: %d", n)
	}
}

func TestSetupIsRateLimited(t *testing.T) {
	e := newSetupEnv(t, "tok", ratelimit.New(nil))
	for i := 0; i < 5; i++ {
		if rec := e.post(setupBody("boss", goodPW, `"setup_token":"wrong"`)); rec.Code != 403 {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	if rec := e.post(setupBody("boss", goodPW, `"setup_token":"tok"`)); rec.Code != 429 {
		t.Fatalf("6th attempt: %d (failed token attempts must count against the limit)", rec.Code)
	}
}

func TestResolveSetupToken(t *testing.T) {
	e := newSetupEnv(t, "", nil)
	ctx := context.Background()
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

	gen, err := ResolveSetupToken(ctx, e.pool, env(nil), "production", log)
	if err != nil || len(gen) < 32 {
		t.Fatalf("generated token %q err=%v", gen, err)
	}
	if strings.Count(buf.String(), gen) != 1 {
		t.Fatalf("token must be logged exactly once, log: %q", buf.String())
	}
	gen2, _ := ResolveSetupToken(ctx, e.pool, env(nil), "production", log)
	if gen2 == gen {
		t.Fatal("token not random per start")
	}
	if got, _ := ResolveSetupToken(ctx, e.pool, env(map[string]string{"SETUP_TOKEN": "fixed"}), "production", log); got != "fixed" {
		t.Fatalf("SETUP_TOKEN override = %q", got)
	}
	if strings.Contains(buf.String(), "fixed") {
		t.Fatal("configured token was logged")
	}
	if got, _ := ResolveSetupToken(ctx, e.pool, env(map[string]string{"SETUP_TOKEN_DISABLED": "1"}), "production", log); got != "" {
		t.Fatalf("disabled = %q", got)
	}
	for _, appEnv := range []string{"dev", "development", "test"} {
		// APP_ENV no longer disables the token; only SETUP_TOKEN_DISABLED does.
		if got, _ := ResolveSetupToken(ctx, e.pool, env(nil), appEnv, log); len(got) < 32 {
			t.Fatalf("APP_ENV=%s must still require a token, got %q", appEnv, got)
		}
	}
	// Once setup is done nothing is generated or logged.
	if rec := e.post(setupBody("boss", goodPW, "")); rec.Code != 200 {
		t.Fatalf("setup: %d", rec.Code)
	}
	buf.Reset()
	if got, _ := ResolveSetupToken(ctx, e.pool, env(nil), "production", log); got != "" || buf.Len() != 0 {
		t.Fatalf("after setup: token=%q log=%q", got, buf.String())
	}
}

// R-marker (final review L1): on a migrated install (users, no setup_complete marker) the API
// start writes the marker, so setup stays closed even if the users table is emptied later.
func TestCloseSetupOnMigratedInstall(t *testing.T) {
	e := newSetupEnv(t, "", nil)
	ctx := context.Background()
	// Greenfield: no user, no marker written.
	if err := CloseSetupIfUsersExist(ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM app_settings WHERE key='setup_complete'`); n != 0 {
		t.Fatal("marker written with no users")
	}
	u, err := e.st.Insert(ctx, identity.NewUser{Username: "legacy", HashedPassword: "x", IsActive: true, Role: "admin", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := CloseSetupIfUsersExist(ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	if err := CloseSetupIfUsersExist(ctx, e.pool); err != nil { // idempotent
		t.Fatal(err)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM app_settings WHERE key='setup_complete'`); n != 1 {
		t.Fatalf("marker rows %d", n)
	}
	if err := e.st.Delete(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if req, _, _ := e.status(t); req {
		t.Fatal("setup re-opened after the users table was emptied")
	}
	if rec := e.post(setupBody("boss", goodPW, "")); rec.Code != 409 {
		t.Fatalf("setup: %d", rec.Code)
	}
}
