//go:build integration

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const ssoPlainSecret = "placeholder-oidc-client-secret"

type ssoEnv struct {
	mcpEnv
	owner *pgxpool.Pool
	ids   map[string]int32
	logs  *bytes.Buffer
}

// newSSOEnv seeds the recorded world: alice owner of ws-alpha (slug alpha), bob a plain member of ws-alpha and owner of
// ws-beta (slug beta), erin an editor of ws-alpha; ws-alpha holds a valid SCIM token (prefix og_scim_contract, expires 2100)
// and ws-beta an expired one. SSO_ALLOWED_ISSUER_HOSTS=login.example.test as in the recording.
func newSSOEnv(t *testing.T) ssoEnv {
	t.Helper()
	t.Setenv("SSO_ALLOWED_ISSUER_HOSTS", "login.example.test")
	pool := testpg.NewAppPool(t, 6)
	owner, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	envl, err := secrets.NewEnvelope(secrets.Config{MasterKey: "sso-routes-test-key", AppEnv: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Envelope: envl, Log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Flags: config.Flags{PGLeadStore: true}}
	registerGovernanceSSO(r, d)
	RegisterSCIM(r, d)
	e := ssoEnv{mcpEnv: mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}, owner: owner, ids: map[string]int32{}, logs: logs}
	e.purge()
	t.Cleanup(e.purge)
	for _, u := range []struct {
		n     string
		admin bool
	}{{"alice", true}, {"bob", false}, {"erin", false}} {
		role := "user"
		if u.admin {
			role = "admin"
		}
		e.seed(t, u.n, pythonHash, true, role, u.admin)
		e.ids[u.n] = e.mustUserByName(t, u.n).ID
	}
	e.workspace(t, "ws-alpha", e.ids["alice"])
	e.workspace(t, "ws-beta", e.ids["bob"])
	e.member(t, "ws-alpha", e.ids["alice"], "owner")
	e.member(t, "ws-alpha", e.ids["bob"], "member")
	e.member(t, "ws-alpha", e.ids["erin"], "editor")
	e.member(t, "ws-beta", e.ids["bob"], "owner")
	e.exec(t, `UPDATE workspaces SET slug='alpha' WHERE id='ws-alpha'`)
	e.exec(t, `UPDATE workspaces SET slug='beta' WHERE id='ws-beta'`)
	e.exec(t, `INSERT INTO workspace_scim_tokens (workspace_id, token_hash, token_prefix, created_at, expires_at, created_by) VALUES ('ws-alpha','seed-hash-alpha','og_scim_contract',1788000000,4102444800,$1)`, e.ids["alice"])
	e.exec(t, `INSERT INTO workspace_scim_tokens (workspace_id, token_hash, token_prefix, created_at, expires_at, created_by) VALUES ('ws-beta','seed-hash-beta','og_scim_contract',1788000000,1788000001,$1)`, e.ids["bob"])
	return e
}

func (e ssoEnv) purge() {
	ctx := context.Background()
	_, _ = e.owner.Exec(ctx, `DELETE FROM workspace_scim_tokens WHERE workspace_id IN ('ws-alpha','ws-beta')`)
	_, _ = e.owner.Exec(ctx, `DELETE FROM workspace_settings WHERE workspace_id IN ('ws-alpha','ws-beta') AND key IN ('OIDC_CONFIG','OIDC_CLIENT_SECRET')`)
}

func (e ssoEnv) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.owner.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (e ssoEnv) str(t *testing.T, sql string, args ...any) (s string) {
	t.Helper()
	if err := e.owner.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return s
}

func (e ssoEnv) req(who, ws, method, path, body string) (int, contract.Resp, string) {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
		hdr["X-Workspace-Id"] = ws
	}
	ctype := ""
	if body != "" {
		ctype = "application/json"
	}
	rec, resp := e.do(method, path, ctype, body, hdr)
	return rec.Code, resp, rec.Body.String()
}

// ssoCases and scimCases are the recorded orders (the state they build on is part of the contract).
var ssoCases = []struct{ name, who, ws string }{
	{"sso_get_default", "alice", "ws-alpha"},
	{"sso_get_bob_403", "bob", "ws-alpha"},
	{"sso_put_erin_403", "erin", "ws-alpha"},
	{"sso_put_defaults", "alice", "ws-alpha"},
	{"sso_put_bad_issuer_http_422", "alice", "ws-alpha"},
	{"sso_put_bad_issuer_host_422", "alice", "ws-alpha"},
	{"sso_put_bad_issuer_query_422", "alice", "ws-alpha"},
	{"sso_put_enforce_without_enable_422", "alice", "ws-alpha"},
	{"sso_put_enabled_without_secret_422", "alice", "ws-alpha"},
	{"sso_put_enabled_without_issuer_422", "alice", "ws-alpha"},
	{"sso_put_bad_role_422", "alice", "ws-alpha"},
	{"sso_put_domains_not_list_422", "alice", "ws-alpha"},
	{"sso_get_after_failures", "alice", "ws-alpha"},
	{"sso_put_valid", "alice", "ws-alpha"},
	{"sso_get_after_valid", "alice", "ws-alpha"},
	{"sso_get_beta_unaffected", "bob", "ws-beta"},
	{"sso_put_keep_existing_secret", "alice", "ws-alpha"},
	{"sso_put_disable", "alice", "ws-alpha"},
	{"sso_put_secret_only_disabled", "alice", "ws-alpha"},
	{"sso_get_final", "alice", "ws-alpha"},
}

var scimCases = []struct{ name, who, ws string }{
	{"scim_get_seeded", "alice", "ws-alpha"},
	{"scim_get_beta_expired", "bob", "ws-beta"},
	{"scim_get_bob_403", "bob", "ws-alpha"},
	{"scim_post_erin_403", "erin", "ws-alpha"},
	{"scim_delete_erin_403", "erin", "ws-alpha"},
	{"scim_post_rotate_201", "alice", "ws-alpha"},
	{"scim_get_after_rotate", "alice", "ws-alpha"},
	{"scim_post_rotate_beta", "bob", "ws-beta"},
	{"scim_get_beta_after_rotate", "bob", "ws-beta"},
	{"scim_delete_204", "alice", "ws-alpha"},
	{"scim_get_after_delete", "alice", "ws-alpha"},
	{"scim_delete_again_204", "alice", "ws-alpha"},
}

func replay(t *testing.T, e ssoEnv, cases []struct{ name, who, ws string }, opts ...contract.Option) {
	for _, c := range cases {
		fx := contract.Load(t, "sso/"+c.name)
		body := ""
		if len(fx.Request.Body) > 0 && string(fx.Request.Body) != "null" {
			body = string(fx.Request.Body)
		}
		_, got, _ := e.req(c.who, c.ws, fx.Request.Method, fx.Request.Path, body)
		t.Run(c.name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response, opts...) })
	}
}

func TestSSORoutesMatchFixtures(t *testing.T) {
	replay(t, newSSOEnv(t), ssoCases)
}

// Seeded ids differ per database, the plaintext token is recorded as <redacted> and the recorded prefix is a counter token.
func TestSCIMTokenRoutesMatchFixturesIgnoringToken(t *testing.T) {
	replay(t, newSSOEnv(t), scimCases,
		contract.IgnoreShapeOf("$.created_by", "$.token", "$.token_prefix", "$.created_at", "$.expires_at"))
}

func TestSSOPutErrorsAre422Strings(t *testing.T) {
	e := newSSOEnv(t)
	for _, c := range []struct{ name, body, detail string }{
		{"http issuer", `{"issuer":"http://login.example.test"}`, "OIDC issuer must be a clean HTTPS origin or path"},
		{"unapproved host", `{"issuer":"https://evil.example.test"}`, "OIDC issuer host is not approved by SSO_ALLOWED_ISSUER_HOSTS"},
		{"query", `{"issuer":"https://login.example.test?x=1"}`, "OIDC issuer must be a clean HTTPS origin or path"},
		{"userinfo", `{"issuer":"https://u:p@login.example.test"}`, "OIDC issuer must be a clean HTTPS origin or path"},
		{"enforce without enable", `{"enforce_sso":true}`, "SSO enforcement requires OIDC to be enabled"},
		{"enabled without secret", `{"enabled":true,"issuer":"https://login.example.test","client_id":"cid"}`, "Enabled OIDC requires issuer, client_id, and client_secret"},
		{"blank secret does not count", `{"enabled":true,"issuer":"https://login.example.test","client_id":"cid","client_secret":"   "}`, "Enabled OIDC requires issuer, client_id, and client_secret"},
	} {
		code, _, raw := e.req("alice", "ws-alpha", "PUT", "/api/governance/sso", c.body)
		var got struct {
			Detail any `json:"detail"`
		}
		if err := json.Unmarshal([]byte(raw), &got); err != nil || code != 422 || got.Detail != c.detail {
			t.Errorf("%s: %d %s", c.name, code, raw)
		}
	}
	if n := e.str(t, `SELECT count(*)::text FROM workspace_settings WHERE workspace_id='ws-alpha' AND key IN ('OIDC_CONFIG','OIDC_CLIENT_SECRET')`); n != "0" {
		t.Errorf("a rejected PUT wrote %s rows", n)
	}
}

func TestSSOPutBodyShapes422(t *testing.T) {
	e := newSSOEnv(t)
	for _, c := range []struct{ name, body, want string }{
		{"no body", ``, `{"detail":[{"type":"missing","loc":["body"],"msg":"Field required","input":null}]}`},
		{"array body", `[1]`, `{"detail":[{"type":"model_attributes_type","loc":["body"],"msg":"Input should be a valid dictionary or object to extract fields from","input":[1]}]}`},
		{"enabled word", `{"enabled":"maybe"}`, `{"detail":[{"type":"bool_parsing","loc":["body","enabled"],"msg":"Input should be a valid boolean, unable to interpret input","input":"maybe"}]}`},
		{"issuer int", `{"issuer":5}`, `{"detail":[{"type":"string_type","loc":["body","issuer"],"msg":"Input should be a valid string","input":5}]}`},
		{"client_secret null", `{"client_secret":null}`, `{"detail":[{"type":"string_type","loc":["body","client_secret"],"msg":"Input should be a valid string","input":null}]}`},
		{"domain item int", `{"allowed_domains":["a.com",3]}`, `{"detail":[{"type":"string_type","loc":["body","allowed_domains",1],"msg":"Input should be a valid string","input":3}]}`},
		{"role int", `{"default_role":1}`, `{"detail":[{"type":"literal_error","loc":["body","default_role"],"msg":"Input should be 'viewer', 'member' or 'editor'","input":1,"ctx":{"expected":"'viewer', 'member' or 'editor'"}}]}`},
		{"two errors in field order", `{"enabled":"x","auto_provision":"y"}`,
			`{"detail":[{"type":"bool_parsing","loc":["body","enabled"],"msg":"Input should be a valid boolean, unable to interpret input","input":"x"},` +
				`{"type":"bool_parsing","loc":["body","auto_provision"],"msg":"Input should be a valid boolean, unable to interpret input","input":"y"}]}`},
	} {
		code, _, raw := e.req("alice", "ws-alpha", "PUT", "/api/governance/sso", c.body)
		if code != 422 || !jsonEqual(raw, c.want) {
			t.Errorf("%s: %d %s", c.name, code, raw)
		}
	}
}

func TestSSOResponseNeverContainsSecret(t *testing.T) {
	e := newSSOEnv(t)
	body := `{"enabled":true,"issuer":"https://login.example.test","client_id":"cid","client_secret":"` + ssoPlainSecret + `"}`
	var seen []string
	_, _, raw := e.req("alice", "ws-alpha", "PUT", "/api/governance/sso", body)
	seen = append(seen, raw)
	_, _, raw = e.req("alice", "ws-alpha", "GET", "/api/governance/sso", "")
	seen = append(seen, raw)
	if !strings.Contains(raw, `"client_secret_configured":true`) {
		t.Errorf("secret not reported configured: %s", raw)
	}
	for _, q := range []string{`SELECT coalesce(string_agg(value,'|'),'') FROM workspace_settings WHERE workspace_id='ws-alpha' AND key='OIDC_CONFIG'`,
		`SELECT coalesce(string_agg(metadata_json::text,'|'),'') FROM governance_audit_events WHERE workspace_id='ws-alpha'`} {
		seen = append(seen, e.str(t, q))
	}
	seen = append(seen, e.logs.String())
	for i, s := range seen {
		if strings.Contains(s, ssoPlainSecret) {
			t.Errorf("surface %d leaks the client secret", i)
		}
	}
	stored := e.str(t, `SELECT value FROM workspace_settings WHERE workspace_id='ws-alpha' AND key='OIDC_CLIENT_SECRET'`)
	if stored == "" || strings.Contains(stored, ssoPlainSecret) {
		t.Errorf("client secret is not stored through the secrets envelope: %q", stored)
	}
	// a PUT that changes only config keeps the secret and a bad PUT never leaks it
	_, _, raw = e.req("alice", "ws-alpha", "PUT", "/api/governance/sso", `{"client_secret":"`+ssoPlainSecret+`","default_role":"admin"}`)
	if strings.Contains(raw, ssoPlainSecret) {
		t.Errorf("422 echoes the secret: %s", raw)
	}
}

func TestSSOTenantIsolation(t *testing.T) {
	e := newSSOEnv(t)
	e.req("alice", "ws-alpha", "PUT", "/api/governance/sso", `{"client_secret":"`+ssoPlainSecret+`","client_id":"alpha-cid"}`)
	_, _, raw := e.req("bob", "ws-beta", "GET", "/api/governance/sso", "")
	if !strings.Contains(raw, `"client_secret_configured":false`) || strings.Contains(raw, "alpha-cid") {
		t.Errorf("beta sees alpha's config: %s", raw)
	}
	// bob owns ws-beta only: pointing his header at alpha is a 403, and alpha's config is untouched.
	if code, _, _ := e.req("bob", "ws-alpha", "PUT", "/api/governance/sso", `{"client_id":"evil"}`); code != 403 {
		t.Errorf("cross-workspace PUT: %d", code)
	}
	_, _, raw = e.req("alice", "ws-alpha", "GET", "/api/governance/sso", "")
	if !strings.Contains(raw, `"client_id":"alpha-cid"`) {
		t.Errorf("alpha config changed: %s", raw)
	}
}

func TestSSORoutesRequireAdmin(t *testing.T) {
	e := newSSOEnv(t)
	routes := []struct{ method, path, body string }{
		{"GET", "/api/governance/sso", ""}, {"PUT", "/api/governance/sso", `{}`},
		{"GET", "/api/governance/scim-token", ""}, {"POST", "/api/governance/scim-token", ""}, {"DELETE", "/api/governance/scim-token", ""},
	}
	for _, r := range routes {
		for _, who := range []string{"bob", "erin"} {
			code, _, raw := e.req(who, "ws-alpha", r.method, r.path, r.body)
			if code != http.StatusForbidden || !strings.Contains(raw, "Workspace permission denied: governance.manage") {
				t.Errorf("%s %s as %s: %d %s", r.method, r.path, who, code, raw)
			}
		}
		if code, _, _ := e.req("", "", r.method, r.path, r.body); code != http.StatusUnauthorized {
			t.Errorf("%s %s anonymous: %d", r.method, r.path, code)
		}
	}
	// nothing the forbidden calls did changed state: the seeded token is still there and still valid
	if n := e.str(t, `SELECT token_hash FROM workspace_scim_tokens WHERE workspace_id='ws-alpha'`); n != "seed-hash-alpha" {
		t.Errorf("a forbidden call changed the token: %s", n)
	}
}

func scimToken(t *testing.T, raw string) string {
	t.Helper()
	var b struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(raw), &b); err != nil || b.Token == "" {
		t.Fatalf("no token in %q", raw)
	}
	return b.Token
}

func (e ssoEnv) scimGet(slug, token string) int {
	rec, _ := e.do("GET", "/scim/v2/"+slug+"/ServiceProviderConfig", "", "", map[string]string{"Authorization": "Bearer " + token})
	return rec.Code
}

func TestSCIMRotateThenOldTokenRejectedByScimRoutes(t *testing.T) {
	e := newSSOEnv(t)
	code, _, raw := e.req("alice", "ws-alpha", "POST", "/api/governance/scim-token", "")
	if code != 201 {
		t.Fatalf("rotate: %d %s", code, raw)
	}
	t1 := scimToken(t, raw)
	if !strings.HasPrefix(t1, "og_scim_") || len(t1) < 40 {
		t.Fatalf("token shape: len %d", len(t1))
	}
	if c := e.scimGet("alpha", t1); c != 200 {
		t.Fatalf("fresh token: %d", c)
	}
	// only a hash is stored
	sum := sha256.Sum256([]byte(t1))
	if h := e.str(t, `SELECT token_hash FROM workspace_scim_tokens WHERE workspace_id='ws-alpha'`); h != hex.EncodeToString(sum[:]) {
		t.Errorf("stored value is not the sha256 digest")
	}
	_, _, raw = e.req("alice", "ws-alpha", "POST", "/api/governance/scim-token", "")
	t2 := scimToken(t, raw)
	if t1 == t2 {
		t.Fatal("rotation returned the same token")
	}
	if c := e.scimGet("alpha", t1); c != 401 {
		t.Errorf("old token after rotate: %d", c)
	}
	if c := e.scimGet("alpha", t2); c != 200 {
		t.Errorf("new token: %d", c)
	}
	if c := e.scimGet("beta", t2); c != 401 {
		t.Errorf("alpha token on beta: %d", c)
	}
	// the raw token is never retrievable afterwards
	_, _, raw = e.req("alice", "ws-alpha", "GET", "/api/governance/scim-token", "")
	if strings.Contains(raw, t2) || strings.Contains(raw, `"token"`) || strings.Contains(raw, "warning") {
		t.Errorf("GET returns token material: %s", raw)
	}
	if code, _, _ := e.req("alice", "ws-alpha", "DELETE", "/api/governance/scim-token", ""); code != 204 {
		t.Fatalf("revoke: %d", code)
	}
	if c := e.scimGet("alpha", t2); c != 401 {
		t.Errorf("revoked token still authenticates: %d", c)
	}
	// revoking in alpha left beta's token row alone
	e.req("bob", "ws-beta", "POST", "/api/governance/scim-token", "")
	e.req("alice", "ws-alpha", "DELETE", "/api/governance/scim-token", "")
	if n := e.str(t, `SELECT count(*)::text FROM workspace_scim_tokens WHERE workspace_id='ws-beta'`); n != "1" {
		t.Errorf("beta token rows: %s", n)
	}
}

func TestSCIMTokenResponseNotLogged(t *testing.T) {
	e := newSSOEnv(t)
	_, _, raw := e.req("alice", "ws-alpha", "POST", "/api/governance/scim-token", "")
	tok := scimToken(t, raw)
	e.req("alice", "ws-alpha", "GET", "/api/governance/scim-token", "")
	e.req("alice", "ws-alpha", "DELETE", "/api/governance/scim-token", "")
	if strings.Contains(e.logs.String(), tok) || strings.Contains(e.logs.String(), tok[8:]) {
		t.Error("the token appears in the logs")
	}
	if a := e.str(t, `SELECT coalesce(string_agg(metadata_json::text || route || resource_path,'|'),'') FROM governance_audit_events WHERE workspace_id='ws-alpha'`); strings.Contains(a, tok) || strings.Contains(a, tok[8:]) {
		t.Error("the token appears in audit events")
	}
}
