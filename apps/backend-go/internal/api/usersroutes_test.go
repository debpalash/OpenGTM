//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func newUsersEnv(t *testing.T) env {
	t.Helper()
	e, _ := newUsersEnvPool(t)
	return e
}

func newUsersEnvPool(t *testing.T) (env, *pgxpool.Pool) {
	t.Helper()
	pool := testpg.NewAppPool(t, 4)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	RegisterUsers(r, Deps{Pool: pool, Signer: testSigner, Identity: st})
	return env{h: r.Handler(), st: st}, pool
}

func bearer(name string) map[string]string {
	tok, _ := testSigner.Access(name, []string{"pwd"})
	return map[string]string{"Authorization": "Bearer " + tok}
}

// seedAdmin seeds an admin and returns its id and auth header.
func (e env) seedAdmin(t *testing.T, name string) (int32, map[string]string) {
	t.Helper()
	e.seed(t, name, pythonHash, true, "admin", true)
	u, err := e.st.ByUsername(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID, bearer(name)
}

func (e env) mustUser(t *testing.T, name string) identity.User {
	t.Helper()
	u, err := e.st.ByUsername(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func byUsername(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatalf("not an array: %v", err)
	}
	out := map[string]json.RawMessage{}
	for _, el := range arr {
		var u struct{ Username string }
		_ = json.Unmarshal(el, &u)
		out[u.Username] = el
	}
	return out
}

func TestListUsersMatchesFixture(t *testing.T) {
	e := newUsersEnv(t)
	_, hdr := e.seedAdmin(t, "alice")
	e.seed(t, "bob", pythonHash, true, "user", false)
	e.seed(t, "carol", pythonHash, false, "user", false)
	fx := contract.Load(t, "users/list_ok")
	_, got := e.do("GET", "/admin/users", "", "", hdr)
	if got.Status != fx.Response.Status {
		t.Fatalf("status %d", got.Status)
	}
	want, have := byUsername(t, fx.Response.Body), byUsername(t, got.Body)
	for _, name := range []string{"alice", "bob", "carol"} {
		w, ok := want[name]
		h, ok2 := have[name]
		if !ok || !ok2 {
			t.Fatalf("%s missing (fixture %v, got %v)", name, ok, ok2)
		}
		contract.AssertEqual(t,
			contract.Resp{Status: 200, Headers: got.Headers, Body: h},
			contract.Resp{Status: 200, Headers: fx.Response.Headers, Body: w},
			contract.IgnorePaths("$.id", "$.last_login", "$.created_at"))
	}
}

func TestNoTokenAndNonAdminMatchFixtures(t *testing.T) {
	e := newUsersEnv(t)
	e.seed(t, "t10-plain", pythonHash, true, "user", false)
	fx := contract.Load(t, "users/list_non_admin")
	_, got := e.do("GET", "/admin/users", "", "", bearer("t10-plain"))
	contract.AssertEqual(t, got, fx.Response)
	fx = contract.Load(t, "users/list_no_token")
	_, got = e.do("GET", "/admin/users", "", "", nil)
	contract.AssertEqual(t, got, fx.Response)
}

func TestNonAdminIs403(t *testing.T) {
	e := newUsersEnv(t)
	e.seed(t, "t10-viewer", pythonHash, true, "viewer", false)
	h := bearer("t10-viewer")
	for _, c := range [][3]string{
		{"GET", "/admin/users", ""}, {"POST", "/admin/users", `{"username":"t10-x","password":"p"}`},
		{"PUT", "/admin/users/1/password", `{"password":"p"}`}, {"DELETE", "/admin/users/1", ""},
		{"PATCH", "/admin/users/1/role", `{"role":"admin"}`},
	} {
		rec, _ := e.do(c[0], c[1], "application/json", c[2], h)
		if rec.Code != 403 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Not authorized"}` {
			t.Fatalf("%s %s: %d %s", c[0], c[1], rec.Code, rec.Body.String())
		}
	}
	if _, err := e.st.ByUsername(context.Background(), "t10-x"); err == nil {
		t.Fatal("non-admin created a user")
	}
}

func TestCreateUserDefaults(t *testing.T) {
	e := newUsersEnv(t)
	_, hdr := e.seedAdmin(t, "t10-adm1")
	rec, _ := e.do("POST", "/admin/users", "application/json", `{"username":"t10-new1","password":"pw-new-1"}`, hdr)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	u := e.mustUser(t, "t10-new1")
	t.Cleanup(func() { _ = e.st.Delete(context.Background(), u.ID) })
	if !u.IsActive || u.IsAdmin || u.Role != "user" || u.CreatedAt == nil {
		t.Fatalf("%+v", u)
	}
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m["username"] != "t10-new1" || m["role"] != "user" || m["is_active"] != true || m["is_admin"] != false {
		t.Fatalf("%v", m)
	}
	if _, has := m["hashed_password"]; has {
		t.Fatal("hash leaked")
	}
}

func TestCreateHashesBcrypt(t *testing.T) {
	e := newUsersEnv(t)
	_, hdr := e.seedAdmin(t, "t10-adm2")
	rec, _ := e.do("POST", "/admin/users", "application/json", `{"username":"t10-bc","password":"pw-bcrypt-1"}`, hdr)
	if rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	u, hash, err := e.st.PasswordHashByUsername(context.Background(), "t10-bc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.st.Delete(context.Background(), u.ID) })
	if !strings.HasPrefix(hash, "$2") || hash == "pw-bcrypt-1" || !auth.VerifyPassword("pw-bcrypt-1", hash) {
		t.Fatal("password not stored as bcrypt")
	}
}

func TestCreateDuplicateIs400(t *testing.T) {
	e := newUsersEnv(t)
	_, hdr := e.seedAdmin(t, "t10-adm3")
	e.seed(t, "t10-dup", pythonHash, true, "user", false)
	fx := contract.Load(t, "users/create_duplicate")
	_, got := e.do("POST", "/admin/users", "application/json", `{"username":"t10-dup","password":"whatever-1"}`, hdr)
	contract.AssertEqual(t, got, fx.Response)
}

func TestCreateMissingFieldsMatchesFixture(t *testing.T) {
	e := newUsersEnv(t)
	_, hdr := e.seedAdmin(t, "t10-adm4")
	fx := contract.Load(t, "users/create_missing_fields")
	_, got := e.do("POST", "/admin/users", "application/json", `{}`, hdr)
	contract.AssertEqual(t, got, fx.Response)
}

func TestResetPassword404(t *testing.T) {
	e := newUsersEnv(t)
	_, hdr := e.seedAdmin(t, "t10-adm5")
	fx := contract.Load(t, "users/password_not_found")
	_, got := e.do("PUT", "/admin/users/9999/password", "application/json", `{"password":"new-pass-1"}`, hdr)
	contract.AssertEqual(t, got, fx.Response)
}

func TestResetPasswordUpdatesHash(t *testing.T) {
	e := newUsersEnv(t)
	_, hdr := e.seedAdmin(t, "t10-adm6")
	e.seed(t, "t10-pw", pythonHash, true, "user", false)
	u := e.mustUser(t, "t10-pw")
	rec, _ := e.do("PUT", fmt.Sprintf("/admin/users/%d/password", u.ID), "application/json", `{"password":"fresh-pass-1"}`, hdr)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"message":"Password updated successfully"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	_, hash, _ := e.st.PasswordHashByUsername(context.Background(), "t10-pw")
	if !auth.VerifyPassword("fresh-pass-1", hash) {
		t.Fatal("hash not updated")
	}
}

func TestDeleteSelfIs400(t *testing.T) {
	e := newUsersEnv(t)
	id, hdr := e.seedAdmin(t, "t10-adm7")
	fx := contract.Load(t, "users/delete_self")
	_, got := e.do("DELETE", fmt.Sprintf("/admin/users/%d", id), "", "", hdr)
	contract.AssertEqual(t, got, fx.Response)
	if _, err := e.st.ByID(context.Background(), id); err != nil {
		t.Fatal("self was deleted")
	}
}

func TestDeleteNotFound(t *testing.T) {
	e := newUsersEnv(t)
	_, hdr := e.seedAdmin(t, "t10-adm8")
	fx := contract.Load(t, "users/delete_not_found")
	_, got := e.do("DELETE", "/admin/users/9999", "", "", hdr)
	contract.AssertEqual(t, got, fx.Response)
}

func TestDeleteCascadesNothingElse(t *testing.T) {
	e, pool := newUsersEnvPool(t)
	_, hdr := e.seedAdmin(t, "t10-adm9")
	e.seed(t, "t10-victim", pythonHash, true, "user", false)
	v := e.mustUser(t, "t10-victim")
	ctx := context.Background()
	wsID := "t10-ws-" + fmt.Sprint(v.ID)
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, slug) VALUES ($1,$2,$2)`, wsID, "t10 ws"); err != nil {
		t.Fatalf("cannot seed workspace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM workspace_members WHERE workspace_id=$1`, wsID)
		_, _ = pool.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, wsID)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1,$2,'viewer')`, wsID, v.ID); err != nil {
		t.Fatalf("cannot seed membership: %v", err)
	}
	rec, _ := e.do("DELETE", fmt.Sprintf("/admin/users/%d", v.ID), "", "", hdr)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"message":"User deleted"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, err := e.st.ByID(ctx, v.ID); err == nil {
		t.Fatal("user still exists")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_members WHERE workspace_id=$1`, wsID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("membership rows touched: n=%d err=%v", n, err)
	}
}

func TestRoleSuperadminKeepsIsAdminFalse(t *testing.T) {
	e := newUsersEnv(t)
	_, hdr := e.seedAdmin(t, "t10-adm10")
	e.seed(t, "t10-role", pythonHash, true, "user", false)
	u := e.mustUser(t, "t10-role")
	path := fmt.Sprintf("/admin/users/%d/role", u.ID)
	rec, _ := e.do("PATCH", path, "application/json", `{"role":"superadmin"}`, hdr)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m["role"] != "superadmin" || m["is_admin"] != false {
		t.Fatalf("%v", m)
	}
	if got := e.mustUser(t, "t10-role"); got.Role != "superadmin" || got.IsAdmin {
		t.Fatalf("%+v", got)
	}
	rec, _ = e.do("PATCH", path, "application/json", `{"role":"admin"}`, hdr)
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if rec.Code != 200 || m["is_admin"] != true {
		t.Fatalf("%d %v", rec.Code, m)
	}
	rec, _ = e.do("PATCH", path, "application/json", `{"role":"viewer"}`, hdr)
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m["is_admin"] != false || m["role"] != "viewer" {
		t.Fatalf("%v", m)
	}
}

func TestRoleInvalidIs400(t *testing.T) {
	e := newUsersEnv(t)
	_, hdr := e.seedAdmin(t, "t10-adm11")
	e.seed(t, "t10-badrole", pythonHash, true, "user", false)
	u := e.mustUser(t, "t10-badrole")
	fx := contract.Load(t, "users/role_invalid")
	_, got := e.do("PATCH", fmt.Sprintf("/admin/users/%d/role", u.ID), "application/json", `{"role":"bogus"}`, hdr)
	contract.AssertEqual(t, got, fx.Response)
	if e.mustUser(t, "t10-badrole").Role != "user" {
		t.Fatal("role changed")
	}
}

func TestRoleNotFoundBeforeInvalid(t *testing.T) {
	e := newUsersEnv(t)
	_, hdr := e.seedAdmin(t, "t10-adm12")
	fx := contract.Load(t, "users/role_not_found")
	_, got := e.do("PATCH", "/admin/users/9999/role", "application/json", `{"role":"admin"}`, hdr)
	contract.AssertEqual(t, got, fx.Response)
	// Python looks the user up before validating the role.
	rec, _ := e.do("PATCH", "/admin/users/9999/role", "application/json", `{"role":"bogus"}`, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestPathIntParsing422(t *testing.T) {
	e := newUsersEnv(t)
	_, hdr := e.seedAdmin(t, "t10-adm13")
	fx := contract.Load(t, "users/path_int_parsing")
	_, got := e.do("DELETE", "/admin/users/abc", "", "", hdr)
	contract.AssertEqual(t, got, fx.Response)
	for _, c := range [][3]string{{"PUT", "/admin/users/abc/password", `{"password":"p"}`}, {"PATCH", "/admin/users/abc/role", `{"role":"admin"}`}} {
		rec, _ := e.do(c[0], c[1], "application/json", c[2], hdr)
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"int_parsing"`) {
			t.Fatalf("%s: %d %s", c[1], rec.Code, rec.Body.String())
		}
	}
}
