//go:build integration

package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// fakeIdP is an https OpenID provider: discovery, JWKS and a token endpoint that answers an ID
// token built from the claims the test sets (nonce filled from the login redirect).
type fakeIdP struct {
	srv     *httptest.Server
	key     *rsa.PrivateKey
	mu      sync.Mutex
	claims  jwt.MapClaims
	gotPKCE string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize",
			"token_endpoint": f.srv.URL + "/token", "jwks_uri": f.srv.URL + "/jwks"})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		id, sec, _ := r.BasicAuth()
		if id != "cid" || sec != "sso-test-secret" || r.FormValue("grant_type") != "authorization_code" || r.FormValue("code") != "good-code" {
			http.Error(w, `{"error":"invalid_grant"}`, 400)
			return
		}
		f.mu.Lock()
		f.gotPKCE = r.FormValue("code_verifier")
		c := jwt.MapClaims{}
		for k, v := range f.claims {
			c[k] = v
		}
		f.mu.Unlock()
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
		tok.Header["kid"] = "k1"
		s, err := tok.SignedString(key)
		if err != nil {
			http.Error(w, "sign", 500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": s})
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

type ssoEnvT struct {
	h    http.Handler
	pool *pgxpool.Pool
	idp  *fakeIdP
	ws   string
	slug string
	tag  string
	ids  map[string]int32
}

// newSSOLoginEnv: workspace <tag>-ws (slug <tag>) with SSO enabled for the fake IdP; users
// owner@<tag>.test (owner), admin@, editor@ (members), outsider@ (member of another workspace only).
func newSSOLoginEnv(t *testing.T, autoProvision bool) *ssoEnvT {
	t.Helper()
	idp := newFakeIdP(t)
	t.Setenv("SSO_ALLOWED_ISSUERS", idp.srv.URL)
	t.Setenv("SSO_ALLOWED_ISSUER_HOSTS", "")
	old := ssoHTTPClient
	ssoHTTPClient = func(Deps) *http.Client { return idp.srv.Client() }
	t.Cleanup(func() { ssoHTTPClient = old })

	pool := testpg.NewAppPool(t, 6)
	st := identity.NewStore(pool)
	envl, err := secrets.NewEnvelope(secrets.Config{MasterKey: "sso-login-it-master", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Envelope: envl, Getenv: func(string) string { return "" }}
	r := httpx.NewRouter()
	RegisterAuth(r, d)
	tag := fmt.Sprintf("ssol%d", time.Now().UnixNano()%1e9)
	e := &ssoEnvT{h: r.Handler(), pool: pool, idp: idp, ws: tag + "-ws", slug: tag, tag: tag, ids: map[string]int32{}}
	ctx := context.Background()
	mk := func(name string) int32 {
		var id int32
		if err := pool.QueryRow(ctx, `INSERT INTO users (username, hashed_password, is_active, is_admin, role) VALUES ($1,'x',true,false,'user') RETURNING id`,
			name+"@"+tag+".test").Scan(&id); err != nil {
			t.Fatal(err)
		}
		e.ids[name] = id
		return id
	}
	for _, n := range []string{"owner", "admin", "editor", "outsider", "viewer2"} {
		mk(n)
	}
	other := tag + "-other"
	for _, w := range []string{e.ws, other} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id,name,slug,description,icon,owner_id,created_at,updated_at) VALUES ($1::text,$2::text,$3::text,'','',$4,1,1)`,
			w, "Name "+w, strings.TrimSuffix(w, "-ws"), e.ids["owner"]); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []struct {
		ws, who, role string
	}{{e.ws, "owner", "owner"}, {e.ws, "admin", "admin"}, {e.ws, "editor", "editor"}, {e.ws, "viewer2", "viewer"}, {other, "outsider", "owner"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_members (workspace_id,user_id,role,created_at) VALUES ($1,$2,$3,1)`, m.ws, e.ids[m.who], m.role); err != nil {
			t.Fatal(err)
		}
	}
	cfg := fmt.Sprintf(`{"enabled":true,"enforce_sso":false,"issuer":%q,"client_id":"cid","allowed_domains":[],"auto_provision":%v,"default_role":"viewer"}`,
		idp.srv.URL, autoProvision)
	if err := tenancy.InWorkspace(ctx, pool, e.ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workspace_settings (workspace_id, key, value) VALUES ($1,'OIDC_CONFIG',$2)`, e.ws, cfg)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res := secrets.NewResolver(envl, secrets.NewPgStore(pool), func(string) string { return "" })
	if err := res.Set(ctx, e.ws, "OIDC_CLIENT_SECRET", "sso-test-secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, w := range []string{e.ws, other} {
			_ = tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
				_, _ = tx.Exec(ctx, `DELETE FROM workspace_settings WHERE workspace_id=$1`, w)
				return nil
			})
			_, _ = pool.Exec(ctx, `DELETE FROM workspace_oidc_identities WHERE workspace_id=$1`, w)
			_, _ = pool.Exec(ctx, `DELETE FROM user_active_workspace WHERE workspace_id=$1`, w)
			_, _ = pool.Exec(ctx, `DELETE FROM workspace_members WHERE workspace_id=$1`, w)
		}
		for _, w := range []string{e.ws, other} {
			_, _ = pool.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, w)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE username LIKE $1`, "%@"+tag+".test")
	})
	return e
}

func (e *ssoEnvT) get(path string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.Host = "app.example.test"
	req.Header.Set("X-Forwarded-Proto", "https")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// start runs /login and returns the flow cookie, state and nonce.
func (e *ssoEnvT) start(t *testing.T) (*http.Cookie, string, string) {
	t.Helper()
	rec := e.get("/auth/sso/"+e.slug+"/login", nil)
	if rec.Code != 302 {
		t.Fatalf("login %d %s", rec.Code, rec.Body)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || !strings.HasPrefix(loc.String(), e.idp.srv.URL+"/authorize?") {
		t.Fatalf("location %q", rec.Header().Get("Location"))
	}
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("redirect_uri") != "https://app.example.test/auth/sso/"+e.slug+"/callback" ||
		q.Get("client_id") != "cid" || q.Get("scope") != "openid email profile" {
		t.Fatalf("authorize params %v", q)
	}
	var ck *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == ssoCookieName {
			ck = c
		}
	}
	if ck == nil || !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/auth/sso/" {
		t.Fatalf("flow cookie %+v", ck)
	}
	return ck, q.Get("state"), q.Get("nonce")
}

func (e *ssoEnvT) claims(sub, email, nonce string, extra map[string]any) {
	now := time.Now()
	c := jwt.MapClaims{"iss": e.idp.srv.URL, "aud": "cid", "sub": sub, "email": email, "email_verified": true,
		"nonce": nonce, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()}
	for k, v := range extra {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	e.idp.mu.Lock()
	e.idp.claims = c
	e.idp.mu.Unlock()
}

// login runs the whole flow and returns the callback redirect fragment values.
func (e *ssoEnvT) login(t *testing.T, sub, email string, extra map[string]any) url.Values {
	t.Helper()
	ck, state, nonce := e.start(t)
	e.claims(sub, email, nonce, extra)
	return e.callback(t, "code=good-code&state="+url.QueryEscape(state), ck)
}

func (e *ssoEnvT) callback(t *testing.T, query string, ck *http.Cookie) url.Values {
	t.Helper()
	var cks []*http.Cookie
	if ck != nil {
		cks = append(cks, ck)
	}
	rec := e.get("/auth/sso/"+e.slug+"/callback?"+query, cks)
	if rec.Code != 303 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("callback %d cc=%q %s", rec.Code, rec.Header().Get("Cache-Control"), rec.Body)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login#") {
		t.Fatalf("callback location %q", loc)
	}
	frag, err := url.ParseQuery(strings.TrimPrefix(loc, "/login#"))
	if err != nil {
		t.Fatal(err)
	}
	return frag
}

func (e *ssoEnvT) bindings(t *testing.T) int {
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM workspace_oidc_identities WHERE workspace_id=$1`, e.ws).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func wantSSOError(t *testing.T, frag url.Values, msg string) {
	t.Helper()
	if frag.Get("sso_access_token") != "" || frag.Get("sso_error") != msg {
		t.Fatalf("want sso_error %q, got %v", msg, frag)
	}
}

func TestSSOStatus(t *testing.T) {
	e := newSSOLoginEnv(t, false)
	rec := e.get("/auth/sso/"+e.slug, nil)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"enabled":true,"workspace":"Name `+e.ws+`"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := e.get("/auth/sso/no-such-slug-x/login", nil); rec.Code != 404 || !strings.Contains(rec.Body.String(), "Workspace not found") {
		t.Fatalf("unknown slug %d %s", rec.Code, rec.Body)
	}
}

// A member (non-privileged) is linked by (workspace, issuer, subject) and later logins use the binding.
func TestSSOLinksWorkspaceMember(t *testing.T) {
	e := newSSOLoginEnv(t, false)
	frag := e.login(t, "sub-editor", "Editor@"+e.tag+".test", nil)
	tok := frag.Get("sso_access_token")
	c, err := testSigner.ParseAccess(tok)
	if err != nil || c.Sub != "editor@"+e.tag+".test" || len(c.AMR) != 1 || c.AMR[0] != "sso" {
		t.Fatalf("token %v %+v %v", frag, c, err)
	}
	if e.idp.gotPKCE == "" {
		t.Fatal("no PKCE verifier sent")
	}
	if n := e.bindings(t); n != 1 {
		t.Fatalf("bindings %d", n)
	}
	// the binding, not the email, decides from now on
	frag = e.login(t, "sub-editor", "renamed@elsewhere.test", nil)
	if c, err := testSigner.ParseAccess(frag.Get("sso_access_token")); err != nil || c.Sub != "editor@"+e.tag+".test" {
		t.Fatalf("second login %v", frag)
	}
	// another subject with the editor's email does not take over the editor
	wantSSOError(t, e.login(t, "sub-other", "editor@"+e.tag+".test", nil), "This account is already linked to another SSO identity")
}

// A deployment user who is not a member of this workspace is never linked (Python linked by global username).
func TestSSONonMemberNotLinked(t *testing.T) {
	for _, auto := range []bool{false, true} {
		t.Run(fmt.Sprint("auto_provision=", auto), func(t *testing.T) {
			e := newSSOLoginEnv(t, auto)
			wantSSOError(t, e.login(t, "sub-out", "outsider@"+e.tag+".test", nil), "This account is not provisioned in the workspace")
			if n := e.bindings(t); n != 0 {
				t.Fatalf("bindings %d", n)
			}
			var members int
			_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM workspace_members WHERE workspace_id=$1 AND user_id=$2`, e.ws, e.ids["outsider"]).Scan(&members)
			if members != 0 {
				t.Fatal("outsider was added to the workspace")
			}
		})
	}
}

// Owners and admins are never linked implicitly.
func TestSSOOwnerAndAdminNotLinked(t *testing.T) {
	e := newSSOLoginEnv(t, true)
	for _, who := range []string{"owner", "admin"} {
		wantSSOError(t, e.login(t, "sub-"+who, who+"@"+e.tag+".test", nil), "SSO cannot be linked automatically to an owner or admin account")
	}
	if n := e.bindings(t); n != 0 {
		t.Fatalf("bindings %d", n)
	}
}

func TestSSOStateMismatchRefused(t *testing.T) {
	e := newSSOLoginEnv(t, false)
	ck, state, nonce := e.start(t)
	e.claims("sub-editor", "editor@"+e.tag+".test", nonce, nil)
	wantSSOError(t, e.callback(t, "code=good-code&state=forged-"+state, ck), "SSO authentication failed")
	wantSSOError(t, e.callback(t, "code=good-code&state="+url.QueryEscape(state), nil), "SSO authentication failed")
	forged := *ck
	forged.Value = ck.Value + "x"
	wantSSOError(t, e.callback(t, "code=good-code&state="+url.QueryEscape(state), &forged), "SSO authentication failed")
	if n := e.bindings(t); n != 0 {
		t.Fatalf("bindings %d", n)
	}
}

// The ID token must come from the stored issuer exactly, for this client, with the login nonce.
func TestSSOIDTokenChecks(t *testing.T) {
	e := newSSOLoginEnv(t, false)
	for name, extra := range map[string]map[string]any{
		"issuer path differs": {"iss": e.idp.srv.URL + "/other-tenant"},
		"audience":            {"aud": "other-client"},
		"nonce":               {"nonce": "not-the-nonce"},
		"expired":             {"exp": time.Now().Add(-10 * time.Minute).Unix()},
	} {
		t.Run(name, func(t *testing.T) {
			wantSSOError(t, e.login(t, "sub-editor", "editor@"+e.tag+".test", extra), "SSO authentication failed")
		})
	}
	wantSSOError(t, e.login(t, "sub-editor", "editor@"+e.tag+".test", map[string]any{"email_verified": "true"}),
		"The identity provider must return a verified email")
	if n := e.bindings(t); n != 0 {
		t.Fatalf("bindings %d", n)
	}
}

// auto_provision creates a brand-new user only, as a member of this workspace with the default role.
func TestSSOAutoProvisionNewUser(t *testing.T) {
	e := newSSOLoginEnv(t, true)
	email := "newbie@" + e.tag + ".test"
	frag := e.login(t, "sub-new", email, nil)
	if c, err := testSigner.ParseAccess(frag.Get("sso_access_token")); err != nil || c.Sub != email {
		t.Fatalf("%v", frag)
	}
	var role string
	if err := e.pool.QueryRow(context.Background(), `SELECT m.role FROM workspace_members m JOIN users u ON u.id=m.user_id WHERE m.workspace_id=$1 AND u.username=$2`,
		e.ws, email).Scan(&role); err != nil || role != "viewer" {
		t.Fatalf("role %q %v", role, err)
	}
}

func TestSSOInactiveMemberRefused(t *testing.T) {
	e := newSSOLoginEnv(t, false)
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET is_active=false WHERE id=$1`, e.ids["viewer2"]); err != nil {
		t.Fatal(err)
	}
	wantSSOError(t, e.login(t, "sub-v2", "viewer2@"+e.tag+".test", nil), "This SSO identity is not an active workspace member")
}

// Implicit first-login linking never reaches an account that matters outside this workspace: a
// member of any other workspace (above all an owner/admin there), a global admin, or the owner of
// any workspace. The workspace's IdP could otherwise mint a session that works elsewhere.
func TestSSONoImplicitLinkOfCrossWorkspaceAccounts(t *testing.T) {
	e := newSSOLoginEnv(t, true)
	ctx := context.Background()
	other := e.tag + "-other"
	third := e.tag + "-third"
	mk := func(name string, admin bool) int32 {
		var id int32
		if err := e.pool.QueryRow(ctx, `INSERT INTO users (username, hashed_password, is_active, is_admin, role) VALUES ($1,'x',true,$2,'user') RETURNING id`,
			name+"@"+e.tag+".test", admin).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := e.pool.Exec(ctx, `INSERT INTO workspace_members (workspace_id,user_id,role,created_at) VALUES ($1,$2,'editor',1)`, e.ws, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	adminElsewhere := mk("adminelsewhere", false)
	memberElsewhere := mk("memberelsewhere", false)
	mk("gadmin", true)
	ownsThird := mk("ownsthird", false)
	for _, m := range []struct {
		uid  int32
		role string
	}{{adminElsewhere, "admin"}, {memberElsewhere, "viewer"}} {
		if _, err := e.pool.Exec(ctx, `INSERT INTO workspace_members (workspace_id,user_id,role,created_at) VALUES ($1,$2,$3,1)`, other, m.uid, m.role); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO workspaces (id,name,slug,description,icon,owner_id,created_at,updated_at) VALUES ($1::text,$1::text,$1::text,'','',$2,1,1)`,
		third, ownsThird); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id=$1`, third) })
	for _, who := range []string{"adminelsewhere", "memberelsewhere", "gadmin", "ownsthird"} {
		frag := e.login(t, "sub-"+who, who+"@"+e.tag+".test", nil)
		if frag.Get("sso_access_token") != "" || frag.Get("sso_error") == "" {
			t.Errorf("%s: linked implicitly: %v", who, frag)
		}
	}
	if n := e.bindings(t); n != 0 {
		t.Fatalf("bindings %d", n)
	}
}
