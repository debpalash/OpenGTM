//go:build integration

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type mcpEnv struct {
	env
	pool *pgxpool.Pool
}

func newMCPEnv(t *testing.T) mcpEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 4)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	RegisterMCPTokens(r, Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{MCPTokenTTLDays: 90}})
	return mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}
}

// workspace creates ws owned by owner (also a member with role ownerRole) and cleans up.
func (e mcpEnv) workspace(t *testing.T, id string, owner int32) {
	t.Helper()
	ctx := context.Background()
	slug := id
	if _, err := e.pool.Exec(ctx, `INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1::varchar,$1::text,$2,$3)`, id, slug, owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_ = tenancy.InWorkspace(c, e.pool, id, func(tx pgx.Tx) error {
			_, _ = tx.Exec(c, `DELETE FROM governance_audit_events WHERE workspace_id=$1`, id)
			_, _ = tx.Exec(c, `DELETE FROM workspace_member_permissions WHERE workspace_id=$1`, id)
			return nil
		})
		_, _ = e.pool.Exec(c, `DELETE FROM mcp_tokens WHERE workspace_id=$1`, id)
		_, _ = e.pool.Exec(c, `DELETE FROM workspace_members WHERE workspace_id=$1`, id)
		_, _ = e.pool.Exec(c, `DELETE FROM workspaces WHERE id=$1`, id)
	})
}

func (e mcpEnv) member(t *testing.T, ws string, uid int32, role string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1,$2,$3)`, ws, uid, role); err != nil {
		t.Fatal(err)
	}
}

// admin sets up an admin member of ws and returns headers for it.
func (e mcpEnv) admin(t *testing.T, ws, name string) (int32, map[string]string) {
	t.Helper()
	e.seed(t, name, pythonHash, true, "user", false)
	u := e.mustUserByName(t, name)
	if !e.wsExists(ws) {
		e.workspace(t, ws, u.ID)
	}
	e.member(t, ws, u.ID, "admin")
	h := bearer(name)
	h["X-Workspace-Id"] = ws
	return u.ID, h
}

func (e mcpEnv) wsExists(ws string) bool {
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM workspaces WHERE id=$1`, ws).Scan(&n)
	return n > 0
}

func (e mcpEnv) mustUserByName(t *testing.T, name string) identity.User {
	t.Helper()
	u, err := e.st.ByUsername(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (e mcpEnv) create(t *testing.T, hdr map[string]string, body string) (int, map[string]any) {
	t.Helper()
	rec, _ := e.do("POST", "/api/mcp/tokens", "application/json", body, hdr)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func (e mcpEnv) dbToken(t *testing.T, id string) (hash string, caps string, exp *string, createdBy int32, revoked bool) {
	t.Helper()
	err := e.pool.QueryRow(context.Background(),
		`SELECT token_hash, capabilities::text, expires_at::text, created_by, revoked_at IS NOT NULL FROM mcp_tokens WHERE id=$1`, id).
		Scan(&hash, &caps, &exp, &createdBy, &revoked)
	if err != nil {
		t.Fatal(err)
	}
	return
}

func TestCreateReturnsPlaintextOnceAndStoresHash(t *testing.T) {
	e := newMCPEnv(t)
	uid, hdr := e.admin(t, "t12-ws1", "t12-adm1")
	code, out := e.create(t, hdr, `{"name":"ci-bot","capabilities":["leads:read","leads:write"],"ttl_days":7}`)
	if code != 200 {
		t.Fatalf("status %d %v", code, out)
	}
	raw, _ := out["token"].(string)
	if !strings.HasPrefix(raw, "ycp_") || len(raw) != 4+43 {
		t.Fatalf("token shape len=%d", len(raw))
	}
	if out["prefix"] != raw[:12] {
		t.Fatal("prefix mismatch")
	}
	sum := sha256.Sum256([]byte(raw))
	hash, caps, exp, createdBy, _ := e.dbToken(t, out["id"].(string))
	if hash != hex.EncodeToString(sum[:]) {
		t.Fatal("stored hash is not sha256 of plaintext")
	}
	if hash == raw || strings.Contains(caps, raw) {
		t.Fatal("plaintext persisted")
	}
	if exp == nil || createdBy != uid {
		t.Fatalf("exp=%v createdBy=%d", exp, createdBy)
	}
	if out["workspace_id"] != "t12-ws1" || out["name"] != "ci-bot" || out["revoked_at"] != nil || out["last_used_at"] != nil {
		t.Fatalf("body %v", out)
	}
	for _, k := range []string{"created_at", "expires_at"} {
		if s, _ := out[k].(string); !strings.HasSuffix(s, "+00:00") {
			t.Fatalf("%s=%v not aware-UTC isoformat", k, out[k])
		}
	}
}

func TestCreateMatchesFixtureShape(t *testing.T) {
	e := newMCPEnv(t)
	_, hdr := e.admin(t, "t12-ws2", "t12-adm2")
	ign := []string{"$.id", "$.prefix", "$.created_at", "$.token", "$.workspace_id"}
	for _, c := range []struct{ fx, body string }{
		{"mcp_tokens/create_ok", `{"name":"ci-bot","capabilities":["leads:read","leads:write","leads:read"],"ttl_days":7}`},
		{"mcp_tokens/create_defaults", `{}`},
		{"mcp_tokens/create_ttl_zero", `{"capabilities":["outreach:send"],"ttl_days":0}`},
	} {
		fx := contract.Load(t, c.fx)
		_, got := e.do("POST", "/api/mcp/tokens", "application/json", c.body, hdr)
		var m map[string]any
		_ = json.Unmarshal(got.Body, &m)
		// the fixture keeps the redacted token as a string; here only its shape matters
		contract.AssertEqual(t, got, fx.Response, contract.IgnorePaths(ign...), contract.IgnoreShapeOf("$.expires_at"))
		if _, ok := m["token"].(string); !ok {
			t.Fatalf("%s: no token", c.fx)
		}
	}
}

func TestCreateUnknownCapabilityIs400ListRepr(t *testing.T) {
	e := newMCPEnv(t)
	_, hdr := e.admin(t, "t12-ws3", "t12-adm3")
	fx := contract.Load(t, "mcp_tokens/create_unknown_caps")
	_, got := e.do("POST", "/api/mcp/tokens", "application/json", `{"capabilities":["zzz","leads:read","bad:cap","zzz"]}`, hdr)
	contract.AssertEqual(t, got, fx.Response)
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM mcp_tokens WHERE workspace_id='t12-ws3'`).Scan(&n)
	if n != 0 {
		t.Fatal("token persisted on 400")
	}
}

func TestCreateValidationMatchesFixtures(t *testing.T) {
	e := newMCPEnv(t)
	_, hdr := e.admin(t, "t12-ws4", "t12-adm4")
	for _, name := range []string{"mcp_tokens/create_name_too_long", "mcp_tokens/create_bad_types"} {
		fx := contract.Load(t, name)
		_, got := e.do("POST", "/api/mcp/tokens", "application/json", string(fx.Request.Body), hdr)
		contract.AssertEqual(t, got, fx.Response)
	}
}

func TestCreateDedupesCapabilities(t *testing.T) {
	e := newMCPEnv(t)
	_, hdr := e.admin(t, "t12-ws5", "t12-adm5")
	_, out := e.create(t, hdr, `{"capabilities":["leads:write","leads:read","leads:write","leads:read"]}`)
	got, _ := json.Marshal(out["capabilities"])
	if string(got) != `["leads:write","leads:read"]` {
		t.Fatalf("%s", got)
	}
	_, caps, _, _, _ := e.dbToken(t, out["id"].(string))
	var stored []string
	_ = json.Unmarshal([]byte(caps), &stored)
	if len(stored) != 2 || stored[0] != "leads:write" {
		t.Fatalf("stored %v", stored)
	}
}

func TestTtlNullUsesDefaultZeroMeansNever(t *testing.T) {
	e := newMCPEnv(t)
	_, hdr := e.admin(t, "t12-ws6", "t12-adm6")
	for _, c := range []struct {
		body  string
		never bool
	}{
		{`{}`, false}, {`{"ttl_days":null}`, false}, {`{"ttl_days":0}`, true}, {`{"ttl_days":-3}`, true}, {`{"ttl_days":5}`, false},
	} {
		code, out := e.create(t, hdr, c.body)
		if code != 200 {
			t.Fatalf("%s: %d", c.body, code)
		}
		if (out["expires_at"] == nil) != c.never {
			t.Fatalf("%s: expires_at=%v", c.body, out["expires_at"])
		}
	}
	var days float64
	_, out := e.create(t, hdr, `{}`)
	_ = e.pool.QueryRow(context.Background(),
		`SELECT round(extract(epoch FROM (expires_at - created_at))/86400) FROM mcp_tokens WHERE id=$1`, out["id"]).Scan(&days)
	if days != 90 {
		t.Fatalf("default ttl days = %v", days)
	}
}

func TestListNeverContainsPlaintext(t *testing.T) {
	e := newMCPEnv(t)
	_, hdr := e.admin(t, "t12-ws7", "t12-adm7")
	_, a := e.create(t, hdr, `{"name":"first"}`)
	_, b := e.create(t, hdr, `{"name":"second"}`)
	rec, _ := e.do("GET", "/api/mcp/tokens", "", "", hdr)
	body := rec.Body.String()
	for _, o := range []map[string]any{a, b} {
		if strings.Contains(body, o["token"].(string)) {
			t.Fatal("list leaks plaintext")
		}
	}
	if strings.Contains(body, `"token"`) || strings.Contains(body, "token_hash") {
		t.Fatal("list leaks token fields")
	}
	var list []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 2 || list[0]["id"] != b["id"] || list[1]["id"] != a["id"] {
		t.Fatalf("not newest first: %v", list)
	}
}

func TestListEmptyIsArray(t *testing.T) {
	e := newMCPEnv(t)
	_, hdr := e.admin(t, "t12-ws8", "t12-adm8")
	rec, _ := e.do("GET", "/api/mcp/tokens", "", "", hdr)
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("%s", rec.Body.String())
	}
}

func TestRevokeIdempotent(t *testing.T) {
	e := newMCPEnv(t)
	_, hdr := e.admin(t, "t12-ws9", "t12-adm9")
	_, out := e.create(t, hdr, `{}`)
	id := out["id"].(string)
	var first string
	for i := 0; i < 2; i++ {
		rec, _ := e.do("DELETE", "/api/mcp/tokens/"+id, "", "", hdr)
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"ok":true,"id":"`+id+`"}` {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		var at string
		_ = e.pool.QueryRow(context.Background(), `SELECT revoked_at::text FROM mcp_tokens WHERE id=$1`, id).Scan(&at)
		if i == 0 {
			first = at
		} else if at != first {
			t.Fatal("second revoke moved revoked_at")
		}
	}
	rec, _ := e.do("GET", "/api/mcp/tokens", "", "", hdr)
	if !strings.Contains(rec.Body.String(), `"revoked_at":"`) {
		t.Fatal("list does not show revoked_at")
	}
	rec, _ = e.do("DELETE", "/api/mcp/tokens/nope", "", "", hdr)
	fx := contract.Load(t, "mcp_tokens/revoke_not_found")
	if rec.Code != fx.Response.Status || strings.TrimSpace(rec.Body.String()) != `{"detail":"token not found"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestRevokeOtherWorkspace404(t *testing.T) {
	e := newMCPEnv(t)
	_, hdrA := e.admin(t, "t12-wsA", "t12-admA")
	_, hdrB := e.admin(t, "t12-wsB", "t12-admB")
	_, out := e.create(t, hdrA, `{}`)
	id := out["id"].(string)
	rec, _ := e.do("DELETE", "/api/mcp/tokens/"+id, "", "", hdrB)
	if rec.Code != 404 || strings.TrimSpace(rec.Body.String()) != `{"detail":"token not found"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, _, _, _, revoked := e.dbToken(t, id); revoked {
		t.Fatal("cross-workspace revoke succeeded")
	}
	rec, _ = e.do("GET", "/api/mcp/tokens", "", "", hdrB)
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("list leaks other workspace: %s", rec.Body.String())
	}
}

func TestEditorForbidden(t *testing.T) {
	e := newMCPEnv(t)
	_, adm := e.admin(t, "t12-ws10", "t12-adm10")
	e.seed(t, "t12-ed", pythonHash, true, "user", false)
	ed := e.mustUserByName(t, "t12-ed")
	e.member(t, "t12-ws10", ed.ID, "editor")
	h := bearer("t12-ed")
	h["X-Workspace-Id"] = "t12-ws10"
	_, out := e.create(t, adm, `{}`)
	for _, c := range [][2]string{{"POST", `{}`}, {"GET", ""}, {"DELETE", ""}} {
		path := "/api/mcp/tokens"
		if c[0] == "DELETE" {
			path += "/" + out["id"].(string)
		}
		rec, _ := e.do(c[0], path, "application/json", c[1], h)
		if rec.Code != 403 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Workspace permission denied: secrets.manage"}` {
			t.Fatalf("%s: %d %s", c[0], rec.Code, rec.Body.String())
		}
	}
	rec, _ := e.do("GET", "/api/mcp/tokens", "", "", nil)
	if rec.Code != 401 {
		t.Fatalf("no token: %d", rec.Code)
	}
}

// TestPythonResolvesGoMintedToken pins the hash vector: Python resolves a token
// by hashlib.sha256(raw.encode()).hexdigest() (services/mcp/auth.py:hash_token).
func TestPythonResolvesGoMintedToken(t *testing.T) {
	// python: hashlib.sha256(b"ycp_test-vector").hexdigest()
	const want = "b9412eb258afb02f8657a2dcb21fbf4e9168ca2dca847ca5623964edc37707fa"
	if got := hashMCPToken("ycp_test-vector"); got != want {
		t.Fatalf("hash %s", got)
	}
	raw, prefix, err := newMCPToken()
	if err != nil || len(raw) != 47 || prefix != raw[:12] || strings.ContainsAny(raw[4:], "+/=") {
		t.Fatalf("token %d %q %v", len(raw), prefix, err)
	}
}

func TestAuditRowWritten(t *testing.T) {
	e := newMCPEnv(t)
	uid, hdr := e.admin(t, "t12-ws11", "t12-adm11")
	_, out := e.create(t, hdr, `{"name":"audited"}`)
	raw := out["token"].(string)
	var n int
	var uidDB int32
	var route, outcome, md string
	err := tenancy.InWorkspace(context.Background(), e.pool, "t12-ws11", func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*), coalesce(max(actor_user_id),0), coalesce(max(route),''), coalesce(max(outcome),''), coalesce(max(metadata_json::text),'')
			 FROM governance_audit_events WHERE workspace_id='t12-ws11' AND method='POST'`).Scan(&n, &uidDB, &route, &outcome, &md)
	})
	if err != nil || n != 1 || uidDB != uid || outcome != "success" || !strings.Contains(route, "/api/mcp/tokens") {
		t.Fatalf("n=%d uid=%d route=%q outcome=%q err=%v", n, uidDB, route, outcome, err)
	}
	if strings.Contains(md, raw) || strings.Contains(md, hashMCPToken(raw)) {
		t.Fatal("audit metadata contains token material")
	}
}
