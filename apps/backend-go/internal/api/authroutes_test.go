//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

var testSigner = auth.Signer{Secret: []byte("test-only-secret"), AccessTTL: time.Hour, RefreshTTL: 2 * time.Hour}

// pythonHash is bcrypt.hashpw(b"py-hash-pass") from Python's bcrypt ($2b$).
const pythonHash = "$2b$04$evf/l9TLJnVNly5eNvqVguqpHwFukwqC6CCZ2uDaJZDaRxNaw3o2a"

type env struct {
	h  http.Handler
	st *identity.Store
}

func newEnv(t *testing.T) env {
	t.Helper()
	pool := testpg.NewAppPool(t, 4)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	RegisterAuth(r, Deps{Pool: pool, Signer: testSigner, Identity: st})
	return env{h: r.Handler(), st: st}
}

func (e env) seed(t *testing.T, name, hash string, active bool, role string, admin bool) {
	t.Helper()
	ctx := context.Background()
	if old, err := e.st.ByUsername(ctx, name); err == nil {
		_ = e.st.Delete(ctx, old.ID)
	}
	u, err := e.st.Insert(ctx, identity.NewUser{Username: name, HashedPassword: hash, IsActive: active, IsAdmin: admin, Role: role})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.st.Delete(context.Background(), u.ID) })
}

func (e env) seedPW(t *testing.T, name, pw string, active bool) {
	t.Helper()
	h, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	e.seed(t, name, h, active, "user", false)
}

func (e env) do(method, path, ctype, body string, hdr map[string]string) (*httptest.ResponseRecorder, contract.Resp) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	h := map[string]string{}
	for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
		if v := rec.Header().Get(k); v != "" {
			h[k] = v
		}
	}
	return rec, contract.Resp{Status: rec.Code, Headers: h, Body: json.RawMessage(rec.Body.Bytes())}
}

func form(kv ...string) string {
	v := url.Values{}
	for i := 0; i < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v.Encode()
}

const formCT = "application/x-www-form-urlencoded"

func (e env) login(t *testing.T, user, pw string) (access, refresh string) {
	t.Helper()
	rec, _ := e.do("POST", "/auth/token", formCT, form("username", user, "password", pw), nil)
	var b struct{ AccessToken, RefreshToken string }
	var m map[string]string
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &m) != nil {
		t.Fatalf("login: %d", rec.Code)
	}
	b.AccessToken, b.RefreshToken = m["access_token"], m["refresh_token"]
	return b.AccessToken, b.RefreshToken
}

func TestTokenSuccessMatchesFixture(t *testing.T) {
	e := newEnv(t)
	e.seedPW(t, "alice", "alice-pass-1", true)
	fx := contract.Load(t, "auth/token_success")
	_, got := e.do("POST", "/auth/token", formCT, form("username", "alice", "password", "alice-pass-1"), nil)
	contract.AssertEqual(t, got, fx.Response, contract.IgnoreShapeOf("$.access_token", "$.refresh_token"))
	u, _ := e.st.ByUsername(context.Background(), "alice")
	if u.LastLogin == nil {
		t.Fatal("last_login not updated")
	}
}

func TestTokenBadPasswordMatchesFixture(t *testing.T) {
	e := newEnv(t)
	e.seedPW(t, "alice", "alice-pass-1", true)
	fx := contract.Load(t, "auth/token_bad_password")
	_, got := e.do("POST", "/auth/token", formCT, form("username", "alice", "password", "wrong-password"), nil)
	contract.AssertEqual(t, got, fx.Response)
	_, got = e.do("POST", "/auth/token", formCT, form("username", "nobody-here", "password", "x"), nil)
	contract.AssertEqual(t, got, fx.Response)
}

func TestTokenMissingFieldIs422Fixture(t *testing.T) {
	e := newEnv(t)
	fx := contract.Load(t, "auth/token_missing_field")
	_, got := e.do("POST", "/auth/token", formCT, form("username", "alice"), nil)
	contract.AssertEqual(t, got, fx.Response)
	// FastAPI treats an empty required form value as missing.
	_, got = e.do("POST", "/auth/token", formCT, form("username", "alice", "password", ""), nil)
	contract.AssertEqual(t, got, fx.Response)
}

func TestTokenAcceptsPythonHash(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "t9-pyhash", pythonHash, true, "user", false)
	rec, _ := e.do("POST", "/auth/token", formCT, form("username", "t9-pyhash", "password", "py-hash-pass"), nil)
	if rec.Code != 200 {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestRefreshRoundTrip(t *testing.T) {
	e := newEnv(t)
	e.seedPW(t, "t9-rt", "pw-1", true)
	_, refresh := e.login(t, "t9-rt", "pw-1")
	rec, _ := e.do("POST", "/auth/refresh", "application/json", `{"refresh_token":"`+refresh+`"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	var m map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m["token_type"] != "bearer" || m["access_token"] == "" || m["refresh_token"] == "" {
		t.Fatalf("body %v", m)
	}
	c, err := testSigner.ParseAny(m["refresh_token"])
	if err != nil || c.Type != "refresh" || len(c.AMR) != 1 || c.AMR[0] != "pwd" {
		t.Fatalf("%+v %v", c, err)
	}
	if c, err = testSigner.ParseAccess(m["access_token"]); err != nil || c.Sub != "t9-rt" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestRefreshRejectsAccessToken(t *testing.T) {
	e := newEnv(t)
	e.seedPW(t, "t9-acc", "pw-1", true)
	access, _ := e.login(t, "t9-acc", "pw-1")
	fx := contract.Load(t, "auth/refresh_invalid")
	cases := map[string]string{"access": access, "garbage": "not-a-jwt"}
	nosub, _ := testSigner.Refresh("", nil)
	cases["empty-sub"] = nosub
	unknown, _ := testSigner.Refresh("t9-nobody", nil)
	cases["unknown-user"] = unknown
	for name, tok := range cases {
		_, got := e.do("POST", "/auth/refresh", "application/json", `{"refresh_token":"`+tok+`"}`, nil)
		t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
	}
}

func TestRefreshMissingFieldIs422Fixture(t *testing.T) {
	e := newEnv(t)
	fx := contract.Load(t, "auth/refresh_missing_field")
	_, got := e.do("POST", "/auth/refresh", "application/json", `{}`, nil)
	contract.AssertEqual(t, got, fx.Response)
}

func TestRefreshRejectsInactiveUser(t *testing.T) {
	e := newEnv(t)
	e.seedPW(t, "t9-off", "pw-1", false)
	tok, _ := testSigner.Refresh("t9-off", []string{"pwd"})
	fx := contract.Load(t, "auth/refresh_invalid")
	_, got := e.do("POST", "/auth/refresh", "application/json", `{"refresh_token":"`+tok+`"}`, nil)
	contract.AssertEqual(t, got, fx.Response)
}

func TestRefreshKeepsSSOAmr(t *testing.T) {
	e := newEnv(t)
	e.seedPW(t, "t9-sso", "pw-1", true)
	tok, _ := testSigner.Refresh("t9-sso", []string{"sso"})
	rec, _ := e.do("POST", "/auth/refresh", "application/json", `{"refresh_token":"`+tok+`"}`, nil)
	var m map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	for _, k := range []string{"access_token", "refresh_token"} {
		c, err := testSigner.ParseAny(m[k])
		if err != nil || len(c.AMR) != 1 || c.AMR[0] != "sso" {
			t.Fatalf("%s: %+v %v", k, c, err)
		}
	}
	// no amr claim defaults to ["pwd"]
	tok, _ = testSigner.Refresh("t9-sso", nil)
	rec, _ = e.do("POST", "/auth/refresh", "application/json", `{"refresh_token":"`+tok+`"}`, nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if c, _ := testSigner.ParseAny(m["access_token"]); len(c.AMR) != 1 || c.AMR[0] != "pwd" {
		t.Fatalf("default amr: %+v", c)
	}
}

func TestMeMatchesFixture(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "alice", pythonHash, true, "admin", true)
	tok, _ := testSigner.Access("alice", []string{"pwd"})
	fx := contract.Load(t, "auth/me_ok")
	_, got := e.do("GET", "/auth/me", "", "", map[string]string{"Authorization": "Bearer " + tok})
	contract.AssertEqual(t, got, fx.Response, contract.IgnorePaths("$.id", "$.last_login", "$.created_at"))
	fx = contract.Load(t, "auth/me_no_token")
	_, got = e.do("GET", "/auth/me", "", "", nil)
	contract.AssertEqual(t, got, fx.Response)
}

func TestTokenAndRefreshAreNotCacheable(t *testing.T) {
	e := newEnv(t)
	e.seedPW(t, "alice", "alice-pass-1", true)
	rec, _ := e.do("POST", "/auth/token", formCT, form("username", "alice", "password", "alice-pass-1"), nil)
	var m map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	for name, r := range map[string]*httptest.ResponseRecorder{"token": rec} {
		if r.Header().Get("Cache-Control") != "no-store" || r.Header().Get("Pragma") != "no-cache" {
			t.Errorf("%s: Cache-Control=%q Pragma=%q", name, r.Header().Get("Cache-Control"), r.Header().Get("Pragma"))
		}
	}
	rec, _ = e.do("POST", "/auth/refresh", "application/json", mustJSON(map[string]string{"refresh_token": m["refresh_token"]}), nil)
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Pragma") != "no-cache" {
		t.Errorf("refresh: %d Cache-Control=%q Pragma=%q", rec.Code, rec.Header().Get("Cache-Control"), rec.Header().Get("Pragma"))
	}
}

func TestTokenEmptyHashRunsDummyVerify(t *testing.T) {
	e := newEnv(t)
	e.seed(t, "nohash", "", true, "user", false)
	calls := 0
	old := dummyVerify
	dummyVerify = func() { calls++ }
	defer func() { dummyVerify = old }()
	fx := contract.Load(t, "auth/token_bad_password")
	_, got := e.do("POST", "/auth/token", formCT, form("username", "nohash", "password", "whatever-1"), nil)
	contract.AssertEqual(t, got, fx.Response)
	if calls != 1 {
		t.Fatalf("DummyVerify calls for empty-hash user = %d, want 1", calls)
	}
	// unknown user also still burns one verify
	e.do("POST", "/auth/token", formCT, form("username", "ghost-user", "password", "x"), nil)
	if calls != 2 {
		t.Fatalf("DummyVerify calls after unknown user = %d, want 2", calls)
	}
}
