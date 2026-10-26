package identity

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
)

var testSigner = auth.Signer{Secret: []byte("test-only-secret"), AccessTTL: time.Hour, RefreshTTL: time.Hour}

func ok200() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
}

func run(mw func(http.Handler) http.Handler, hdr string, ctxUser *User) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/auth/me", nil)
	if hdr != "" {
		req.Header.Set("Authorization", hdr)
	}
	if ctxUser != nil {
		req = req.WithContext(WithPrincipal(req.Context(), Principal{User: *ctxUser}))
	}
	rec := httptest.NewRecorder()
	mw(ok200()).ServeHTTP(rec, req)
	return rec
}

func TestMissingBearerIs401WithWWWAuthenticate(t *testing.T) {
	fx := contract.Load(t, "auth/me_no_token")
	for _, h := range []string{"", "Basic abc", "Bearer", "Bearer "} {
		rec := run(Authenticate(testSigner, nil), h, nil)
		contract.AssertEqual(t, contract.Resp{
			Status:  rec.Code,
			Headers: map[string]string{"content-type": rec.Header().Get("Content-Type"), "www-authenticate": rec.Header().Get("WWW-Authenticate")},
			Body:    json.RawMessage(rec.Body.Bytes()),
		}, fx.Response)
	}
}

func TestBadTokenIs401(t *testing.T) {
	rec := run(Authenticate(testSigner, nil), "Bearer garbage", nil)
	if rec.Code != 401 || rec.Body.String() != `{"detail":"Could not validate credentials"}` || rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestRefreshTokenOnAPIIs401(t *testing.T) {
	tok, _ := testSigner.Refresh("alice", nil)
	rec := run(Authenticate(testSigner, nil), "Bearer "+tok, nil)
	if rec.Code != 401 || rec.Body.String() != `{"detail":"Could not validate credentials"}` {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestNonAdminIs403AdminGate(t *testing.T) {
	rec := run(RequireGlobalAdmin(), "", &User{Username: "bob", Role: "user"})
	if rec.Code != 403 || rec.Body.String() != `{"detail":"Not authorized"}` {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestSuperadminRoleAllowed(t *testing.T) {
	for _, u := range []User{{Role: "superadmin"}, {Role: "admin"}, {IsAdmin: true}} {
		if rec := run(RequireGlobalAdmin(), "", &u); rec.Code != 200 {
			t.Fatalf("%+v got %d", u, rec.Code)
		}
	}
}

func TestQueryTokenMissingAndRefresh(t *testing.T) {
	_, err := AuthenticateQueryToken(testSigner, nil, "", false)
	if e, _ := err.(*QueryTokenError); e == nil || e.Status != 401 || e.Detail != "Missing token" {
		t.Fatalf("missing: %v", err)
	}
	tok, _ := testSigner.Refresh("alice", nil)
	_, err = AuthenticateQueryToken(testSigner, nil, tok, false)
	if e, _ := err.(*QueryTokenError); e == nil || e.Status != 401 || e.Detail != "Invalid token" {
		t.Fatalf("refresh: %v", err)
	}
}

func TestUserJSONShapeMatchesFixture(t *testing.T) {
	fx := contract.Load(t, "auth/me_ok")
	c := time.Date(2026, 8, 29, 10, 40, 0, 0, time.UTC)
	l := time.Date(2026, 10, 26, 6, 43, 2, 888558000, time.UTC)
	b, err := json.Marshal(User{ID: 1, Username: "alice", IsActive: true, IsAdmin: true, Role: "admin", LastLogin: &l, CreatedAt: &c})
	if err != nil {
		t.Fatal(err)
	}
	contract.AssertEqual(t, contract.Resp{Status: 200, Headers: map[string]string{"content-type": "application/json"}, Body: b}, fx.Response)
	if string(b) != `{"id":1,"username":"alice","is_active":true,"is_admin":true,"role":"admin","profile_image":null,"last_login":"2026-10-26T06:43:02.888558","created_at":"2026-08-29T10:40:00"}` {
		t.Fatalf("exact: %s", b)
	}
}
