//go:build integration

package copilotauth

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type env struct {
	pool *pgxpool.Pool
	st   *identity.Store
}

func newEnv(t *testing.T) env {
	t.Helper()
	pool := testpg.NewAppPool(t, 2)
	return env{pool: pool, st: identity.NewStore(pool)}
}

func (e env) deps(requireAuth bool) Deps {
	return Deps{Signer: testSigner, Identity: e.st, DB: e.pool, RequireAuth: requireAuth}
}

func (e env) user(t *testing.T, name string, active bool) identity.User {
	t.Helper()
	u, err := e.st.Insert(context.Background(), identity.NewUser{Username: name, HashedPassword: "x", IsActive: active, Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.st.Delete(context.Background(), u.ID) })
	return u
}

func (e env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (e env) workspace(t *testing.T, id, slug string, owner *identity.User) {
	t.Helper()
	var o any
	if owner != nil {
		o = owner.ID
	}
	e.exec(t, `INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1::varchar,$1::text,$2,$3)`, id, slug, o)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = e.pool.Exec(c, `DELETE FROM user_active_workspace WHERE workspace_id=$1`, id)
		_, _ = e.pool.Exec(c, `DELETE FROM workspace_members WHERE workspace_id=$1`, id)
		_, _ = e.pool.Exec(c, `DELETE FROM workspaces WHERE id=$1`, id)
	})
}

func (e env) member(t *testing.T, ws string, u identity.User) {
	t.Helper()
	e.exec(t, `INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1,$2,'viewer')`, ws, u.ID)
}

func (e env) active(t *testing.T, ws string, u identity.User) {
	t.Helper()
	e.exec(t, `INSERT INTO user_active_workspace (user_id, workspace_id) VALUES ($1,$2) ON CONFLICT (user_id) DO UPDATE SET workspace_id=EXCLUDED.workspace_id`, u.ID, ws)
}

func (e env) request(name, wsHeader string) *http.Request {
	tok, _ := testSigner.Access(name, nil)
	r := req("Bearer " + tok)
	if wsHeader != "" {
		r.Header.Set("X-Workspace-Id", wsHeader)
	}
	return r
}

func TestInactiveUser401(t *testing.T) {
	e := newEnv(t)
	e.user(t, "t10-inactive", false)
	_, err := Resolve(e.request("t10-inactive", ""), e.deps(true))
	wantBearer(t, err)
}

func TestUnknownUser401(t *testing.T) {
	e := newEnv(t)
	_, err := Resolve(e.request("t10-nobody", ""), e.deps(true))
	wantBearer(t, err)
}

func TestNoWorkspace403(t *testing.T) {
	e := newEnv(t)
	e.user(t, "t10-lonely", true)
	_, err := Resolve(e.request("t10-lonely", ""), e.deps(true))
	wantErr(t, err, 403, "No accessible workspace. Ask an admin to add you to one.")
}

func TestNonMemberAndMissing403SameBody(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "t10-alice", true)
	bob := e.user(t, "t10-bob", true)
	e.workspace(t, "t10-ws-a", "t10-slug-a", &alice)
	e.workspace(t, "t10-ws-b", "t10-slug-b", &bob)
	e.active(t, "t10-ws-a", alice)
	_, foreign := Resolve(e.request("t10-alice", "t10-ws-b"), e.deps(true))
	_, missing := Resolve(e.request("t10-alice", "t10-ws-nope"), e.deps(true))
	wantErr(t, foreign, 403, "Workspace access denied")
	wantErr(t, missing, 403, "Workspace access denied")
}

func TestSlugMissing404(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "t10-slugless", true)
	e.workspace(t, "t10-ws-noslug", "", &alice)
	_, err := Resolve(e.request("t10-slugless", "t10-ws-noslug"), e.deps(true))
	wantErr(t, err, 404, "Workspace not found")
}

func TestOwnerIsMember(t *testing.T) {
	e := newEnv(t)
	owner := e.user(t, "t10-owner", true) // owner_id only, no member row
	e.workspace(t, "t10-ws-own", "t10-slug-own", &owner)
	got, err := Resolve(e.request("t10-owner", "t10-ws-own"), e.deps(true))
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkspaceID != "t10-ws-own" || got.Slug != "t10-slug-own" || got.UserID == nil || *got.UserID != owner.ID || got.IsSelfHost() {
		t.Fatalf("%+v", got)
	}
	if id, perr := got.ToolUserID(); perr != nil || id == nil || *id != owner.ID {
		t.Fatalf("tool user: %v %v", id, perr)
	}
}

func TestHeaderOverridesActive(t *testing.T) {
	e := newEnv(t)
	u := e.user(t, "t10-multi", true)
	e.workspace(t, "t10-ws-1", "t10-slug-1", nil)
	e.workspace(t, "t10-ws-2", "t10-slug-2", nil)
	e.member(t, "t10-ws-1", u)
	e.member(t, "t10-ws-2", u)
	e.active(t, "t10-ws-1", u)
	got, err := Resolve(e.request("t10-multi", ""), e.deps(true))
	if err != nil || got.WorkspaceID != "t10-ws-1" {
		t.Fatalf("active: %+v %v", got, err)
	}
	got, err = Resolve(e.request("t10-multi", "t10-ws-2"), e.deps(true))
	if err != nil || got.WorkspaceID != "t10-ws-2" || got.Slug != "t10-slug-2" {
		t.Fatalf("header: %+v %v", got, err)
	}
}

func TestSelfHostBindsActiveWorkspace(t *testing.T) {
	e := newEnv(t)
	e.workspace(t, "t10-ws-self", "t10-slug-self", nil)
	prev, perr := currentSetting(e)
	e.exec(t, `INSERT INTO app_settings (key, value) VALUES ('ACTIVE_WORKSPACE','t10-ws-self') ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`)
	t.Cleanup(func() {
		if perr != nil {
			_, _ = e.pool.Exec(context.Background(), `DELETE FROM app_settings WHERE key='ACTIVE_WORKSPACE'`)
			return
		}
		_, _ = e.pool.Exec(context.Background(), `UPDATE app_settings SET value=$1 WHERE key='ACTIVE_WORKSPACE'`, prev)
	})
	// garbage Authorization is ignored in self-host mode
	r := req("Bearer garbage")
	got, err := Resolve(r, e.deps(false))
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkspaceID != "t10-ws-self" || got.Slug != "t10-slug-self" || got.UserID != nil || !got.IsSelfHost() {
		t.Fatalf("%+v", got)
	}
	if id, terr := got.ToolUserID(); terr != nil || id != nil {
		t.Fatalf("self-host tool user: %v %v", id, terr)
	}
}

func TestSelfHostSlugFallbackMain(t *testing.T) {
	e := newEnv(t)
	prev, perr := currentSetting(e)
	e.exec(t, `INSERT INTO app_settings (key, value) VALUES ('ACTIVE_WORKSPACE','t10-ws-ghost') ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`)
	t.Cleanup(func() {
		if perr != nil {
			_, _ = e.pool.Exec(context.Background(), `DELETE FROM app_settings WHERE key='ACTIVE_WORKSPACE'`)
			return
		}
		_, _ = e.pool.Exec(context.Background(), `UPDATE app_settings SET value=$1 WHERE key='ACTIVE_WORKSPACE'`, prev)
	})
	got, err := Resolve(req(""), e.deps(false))
	if err != nil || got.WorkspaceID != "t10-ws-ghost" || got.Slug != "main" {
		t.Fatalf("%+v %v", got, err)
	}
}

func currentSetting(e env) (string, error) {
	var v string
	err := e.pool.QueryRow(context.Background(), `SELECT value FROM app_settings WHERE key='ACTIVE_WORKSPACE'`).Scan(&v)
	return v, err
}
