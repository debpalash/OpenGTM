package copilotauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
)

var testSigner = auth.Signer{Secret: []byte("test-only-secret"), AccessTTL: time.Hour, RefreshTTL: time.Hour}

func req(authz string) *http.Request {
	r := httptest.NewRequest("POST", "/api/copilotkit", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	return r
}

func wantErr(t *testing.T, e *Err, status int, detail string) {
	t.Helper()
	if e == nil || e.Status != status || e.Detail != detail {
		t.Fatalf("got %+v, want %d %q", e, status, detail)
	}
}

func wantBearer(t *testing.T, e *Err) {
	t.Helper()
	wantErr(t, e, 401, "Could not validate credentials")
	rec := httptest.NewRecorder()
	e.Write(rec)
	if rec.Header().Get("WWW-Authenticate") != "Bearer" ||
		rec.Body.String() != `{"detail":"Could not validate credentials"}` {
		t.Fatalf("write: %v %s", rec.Header(), rec.Body.String())
	}
}

func TestMissingBearer401WithWWWAuthenticate(t *testing.T) {
	d := Deps{Signer: testSigner, RequireAuth: true}
	for _, h := range []string{"", "Basic abc", "Bearer", "Bearer ", "bearer"} {
		_, e := Resolve(req(h), d)
		wantBearer(t, e)
	}
}

func TestGarbageAndWrongSecretToken401(t *testing.T) {
	d := Deps{Signer: testSigner, RequireAuth: true}
	_, e := Resolve(req("Bearer not.a.jwt"), d)
	wantBearer(t, e)
	other := auth.Signer{Secret: []byte("other"), AccessTTL: time.Hour}
	tok, _ := other.Access("alice", nil)
	_, e = Resolve(req("Bearer "+tok), d)
	wantBearer(t, e)
}

func TestRefreshTokenRejected(t *testing.T) {
	d := Deps{Signer: testSigner, RequireAuth: true}
	tok, _ := testSigner.Refresh("alice", nil)
	_, e := Resolve(req("Bearer "+tok), d)
	wantBearer(t, e)
}

func TestToolUserIDFailsClosed(t *testing.T) {
	// A zero or hand-built Resolved must never yield "no user" silently.
	if _, err := (Resolved{WorkspaceID: "w"}).ToolUserID(); err == nil {
		t.Fatal("zero-user cloud Resolved must error")
	}
	id := int32(7)
	got, err := (Resolved{WorkspaceID: "w", UserID: &id}).ToolUserID()
	if err != nil || got == nil || *got != 7 {
		t.Fatalf("got %v %v", got, err)
	}
	if (Resolved{WorkspaceID: "w"}).IsSelfHost() {
		t.Fatal("hand-built Resolved must not be self-host")
	}
}
