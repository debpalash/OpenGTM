//go:build integration

package identity

import (
	"context"
	"net/http"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func seed(t *testing.T, st *Store, name string, active bool, role string) {
	t.Helper()
	ctx := context.Background()
	u, err := st.Insert(ctx, NewUser{Username: name, HashedPassword: "x", IsActive: active, Role: role})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Delete(context.Background(), u.ID) })
}

func TestInactiveUserIs400(t *testing.T) {
	st := NewStore(testpg.NewAppPool(t, 2))
	seed(t, st, "t4-inactive", false, "user")
	tok, _ := testSigner.Access("t4-inactive", nil)
	rec := run(Authenticate(testSigner, st), "Bearer "+tok, nil)
	if rec.Code != 400 || rec.Body.String() != `{"detail":"Inactive user"}` {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestUnknownUserIs401(t *testing.T) {
	st := NewStore(testpg.NewAppPool(t, 2))
	tok, _ := testSigner.Access("t4-nobody", nil)
	rec := run(Authenticate(testSigner, st), "Bearer "+tok, nil)
	if rec.Code != 401 || rec.Body.String() != `{"detail":"Could not validate credentials"}` || rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestActiveUserAuthenticatesAndTimestamps(t *testing.T) {
	ctx := context.Background()
	st := NewStore(testpg.NewAppPool(t, 2))
	seed(t, st, "t4-active", true, "admin")
	u, _ := st.ByUsername(ctx, "t4-active")
	if err := st.TouchLastLogin(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	tok, _ := testSigner.Access("t4-active", []string{"pwd"})
	var got Principal
	h := Authenticate(testSigner, st)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = PrincipalFrom(r.Context())
	}))
	rec := run(func(http.Handler) http.Handler { return h }, "Bearer "+tok, nil)
	if rec.Code != 200 || got.User.Username != "t4-active" || got.User.LastLogin == nil || got.Claims.AMR[0] != "pwd" {
		t.Fatalf("got %d %+v", rec.Code, got)
	}
}

func TestQueryTokenRejectsRefreshAndInactive(t *testing.T) {
	st := NewStore(testpg.NewAppPool(t, 2))
	seed(t, st, "t4-qinactive", false, "user")
	seed(t, st, "t4-qplain", true, "user")
	for name, tok := range map[string]string{
		"inactive": mustAccess("t4-qinactive"),
		"refresh":  mustRefresh("t4-qplain"),
	} {
		_, err := AuthenticateQueryToken(testSigner, st, tok, false)
		if e, _ := err.(*QueryTokenError); e == nil || e.Status != 401 || e.Detail != "Invalid token" {
			t.Fatalf("%s: %v", name, err)
		}
	}
	_, err := AuthenticateQueryToken(testSigner, st, mustAccess("t4-qplain"), true)
	if e, _ := err.(*QueryTokenError); e == nil || e.Status != 403 {
		t.Fatalf("admin gate: %v", err)
	}
	if _, err := AuthenticateQueryToken(testSigner, st, mustAccess("t4-qplain"), false); err != nil {
		t.Fatal(err)
	}
}

func mustAccess(s string) string  { t, _ := testSigner.Access(s, nil); return t }
func mustRefresh(s string) string { t, _ := testSigner.Refresh(s, nil); return t }
