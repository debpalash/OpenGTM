//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type wsEnv struct {
	env
	pool *pgxpool.Pool
}

func newWsEnv(t *testing.T) wsEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 4)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	RegisterWorkspaces(r, Deps{Pool: pool, Signer: testSigner, Identity: st})
	return wsEnv{env: env{h: r.Handler(), st: st}, pool: pool}
}

// user seeds a user and returns its id and bearer header.
func (e wsEnv) user(t *testing.T, name string) (int32, map[string]string) {
	t.Helper()
	e.seed(t, name, pythonHash, true, "user", false)
	return e.mustUser(t, name).ID, bearer(name)
}

func (e wsEnv) mustUser(t *testing.T, name string) identity.User {
	t.Helper()
	u, err := e.st.ByUsername(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// purge removes a workspace and its identity-plane children (test cleanup).
func (e wsEnv) purge(id string) {
	ctx := context.Background()
	_ = tenancy.InWorkspace(ctx, e.pool, id, func(tx pgx.Tx) error {
		for _, tbl := range []string{"workspace_settings", "workspace_scim_users", "workspace_scim_group_members",
			"workspace_scim_groups", "workspace_member_permissions", "leads", "workspace_oidc_identities",
			"workspace_scim_tokens", "workspace_members", "user_active_workspace"} {
			_, _ = tx.Exec(ctx, "DELETE FROM "+tbl+" WHERE workspace_id = $1", id)
		}
		_, _ = tx.Exec(ctx, "DELETE FROM workspaces WHERE id = $1", id)
		return nil
	})
}

// mk inserts a workspace; members maps user id -> role.
func (e wsEnv) mk(t *testing.T, id, name, slug string, owner int32, created float64, members map[int32]string) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO workspaces (id,name,slug,owner_id,created_at,updated_at) VALUES ($1,$2,$3,$4,$5,$5)`,
		id, name, slug, owner, created); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.purge(id) })
	for uid, role := range members {
		if _, err := e.pool.Exec(ctx, `INSERT INTO workspace_members (workspace_id,user_id,role,created_at) VALUES ($1,$2,$3,$4)`,
			id, uid, role, created); err != nil {
			t.Fatal(err)
		}
	}
}

func (e wsEnv) setActive(t *testing.T, uid int32, ws string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO user_active_workspace (user_id,workspace_id) VALUES ($1,$2)
		ON CONFLICT (user_id) DO UPDATE SET workspace_id=EXCLUDED.workspace_id`, uid, ws); err != nil {
		t.Fatal(err)
	}
}

func (e wsEnv) active(t *testing.T, uid int32) string {
	t.Helper()
	var id string
	err := e.pool.QueryRow(context.Background(), `SELECT workspace_id FROM user_active_workspace WHERE user_id=$1`, uid).Scan(&id)
	if err != nil {
		return ""
	}
	return id
}

func (e wsEnv) exists(t *testing.T, id string) bool {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM workspaces WHERE id=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func (e wsEnv) addLeads(t *testing.T, ws string, n int) {
	t.Helper()
	err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
		for i := 0; i < n; i++ {
			if _, err := tx.Exec(context.Background(), `INSERT INTO leads (workspace_id, company) VALUES ($1, $2)`, ws, fmt.Sprintf("t11-co-%d", i)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func uniq(t *testing.T) string {
	return fmt.Sprintf("t11-%d", time.Now().UnixNano()%1e12)
}

func decodeMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return m
}

func TestListMatchesFixtureEpochFloats(t *testing.T) {
	e := newWsEnv(t)
	uid, hdr := e.user(t, "t11-list")
	n := uniq(t)
	e.mk(t, n+"-a", "Alpha", n+"-alpha", uid, 1788000000, map[int32]string{uid: "owner"})
	e.mk(t, n+"-b", "Beta", n+"-beta", uid, 1788000001, map[int32]string{uid: "member"})
	e.setActive(t, uid, n+"-b")
	rec, got := e.do("GET", "/api/workspaces", "", "", hdr)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	body := decodeMap(t, rec.Body.Bytes())
	if body["active_id"] != n+"-b" {
		t.Fatalf("active_id %v", body["active_id"])
	}
	list := body["workspaces"].([]any)
	if len(list) != 2 {
		t.Fatalf("%v", list)
	}
	fx := contract.Load(t, "workspaces/list_after_create")
	var fb struct{ Workspaces []json.RawMessage }
	_ = json.Unmarshal(fx.Response.Body, &fb)
	var rawList struct{ Workspaces []json.RawMessage }
	_ = json.Unmarshal(rec.Body.Bytes(), &rawList)
	first := rawList.Workspaces[0]
	// fixture entry 0 is "Alpha" (epoch float 1788000000.0, is_active false)
	contract.AssertEqual(t, contract.Resp{Status: 200, Headers: got.Headers, Body: first},
		contract.Resp{Status: 200, Headers: got.Headers, Body: fb.Workspaces[0]},
		contract.IgnorePaths("$.id", "$.slug", "$.owner_id"))
	if _, isNum := list[0].(map[string]any)["created_at"].(float64); !isNum {
		t.Fatal("created_at must be a JSON number")
	}
	if list[1].(map[string]any)["is_active"] != true || list[0].(map[string]any)["is_active"] != false {
		t.Fatal("is_active flags wrong")
	}
	// headers/status of the whole response also match the fixture
	contract.AssertEqual(t, contract.Resp{Status: got.Status, Headers: got.Headers, Body: json.RawMessage(`null`)},
		contract.Resp{Status: fx.Response.Status, Headers: fx.Response.Headers, Body: json.RawMessage(`null`)})
}

func TestListNoTokenMatchesFixture(t *testing.T) {
	e := newWsEnv(t)
	fx := contract.Load(t, "workspaces/list_no_token")
	_, got := e.do("GET", "/api/workspaces", "", "", nil)
	contract.AssertEqual(t, got, fx.Response)
}

func TestListActiveFallsBackToFirstAccessible(t *testing.T) {
	e := newWsEnv(t)
	uid, hdr := e.user(t, "t11-fb")
	n := uniq(t)
	e.mk(t, n+"-z", "Z", n+"-z", uid, 1788000000, map[int32]string{uid: "owner"})
	e.mk(t, n+"-a", "A", n+"-a", uid, 1788000001, map[int32]string{uid: "member"})
	_, other := e.user(t, "t11-fb-o")
	_ = other
	rec, _ := e.do("GET", "/api/workspaces", "", "", hdr)
	if got := decodeMap(t, rec.Body.Bytes())["active_id"]; got != n+"-a" {
		t.Fatalf("expected lowest id, got %v", got)
	}
}

func TestCreateSlugRule(t *testing.T) {
	e := newWsEnv(t)
	_, hdr := e.user(t, "t11-slug")
	for _, c := range []struct{ name, slug string }{
		{"My Team_2!", "my-team-2"}, {"  A b ", "--a-b-"}, {"Ünï-cødé 9", "ünï-cødé-9"}, {"!!!", ""},
	} {
		// make the slug unique per run while preserving the rule under test
		suffix := fmt.Sprintf("x%d", time.Now().UnixNano()%1e9)
		rec, _ := e.do("POST", "/api/workspaces", "application/json",
			mustJSON(map[string]string{"name": c.name + suffix}), hdr)
		if rec.Code != 200 {
			t.Fatalf("%q: %d %s", c.name, rec.Code, rec.Body.String())
		}
		m := decodeMap(t, rec.Body.Bytes())
		id := m["id"].(string)
		t.Cleanup(func() { e.purge(id) })
		if m["slug"] != c.slug+suffix {
			t.Fatalf("%q -> %v want %q", c.name, m["slug"], c.slug+suffix)
		}
		if m["name"] != c.name+suffix || len(m) != 3 {
			t.Fatalf("%v", m)
		}
	}
	// Fixture parity for the documented example.
	fx := contract.Load(t, "workspaces/create_ok")
	rec, got := e.do("POST", "/api/workspaces", "application/json", `{"name":"My Team_2!","description":"d","icon":"T"}`, hdr)
	if rec.Code == 200 {
		t.Cleanup(func() { e.purge(decodeMap(t, rec.Body.Bytes())["id"].(string)) })
	}
	if rec.Code == 409 { // slug left over from a previous run
		return
	}
	contract.AssertEqual(t, got, fx.Response, contract.IgnorePaths("$.id"))
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestCreateDefaultsAndValidation(t *testing.T) {
	e := newWsEnv(t)
	_, hdr := e.user(t, "t11-def")
	fx := contract.Load(t, "workspaces/create_missing_name")
	_, got := e.do("POST", "/api/workspaces", "application/json", `{}`, hdr)
	contract.AssertEqual(t, got, fx.Response)
	name := uniq(t)
	rec, _ := e.do("POST", "/api/workspaces", "application/json", mustJSON(map[string]string{"name": name}), hdr)
	id := decodeMap(t, rec.Body.Bytes())["id"].(string)
	t.Cleanup(func() { e.purge(id) })
	var desc, icon string
	var owner int32
	var created, updated float64
	if err := e.pool.QueryRow(context.Background(), `SELECT description, icon, owner_id, created_at, updated_at FROM workspaces WHERE id=$1`, id).
		Scan(&desc, &icon, &owner, &created, &updated); err != nil {
		t.Fatal(err)
	}
	if desc != "" || icon != "🏢" || created != updated || time.Since(time.Unix(int64(created), 0)) > time.Minute {
		t.Fatalf("%q %q %v %v", desc, icon, created, updated)
	}
}

func TestCreateSetsOwnerAndActive(t *testing.T) {
	e := newWsEnv(t)
	uid, hdr := e.user(t, "t11-own")
	other := uniq(t) + "-prev"
	e.mk(t, other, "Prev", other, uid, 1788000000, map[int32]string{uid: "owner"})
	e.setActive(t, uid, other)
	rec, _ := e.do("POST", "/api/workspaces", "application/json", mustJSON(map[string]string{"name": uniq(t)}), hdr)
	id := decodeMap(t, rec.Body.Bytes())["id"].(string)
	t.Cleanup(func() { e.purge(id) })
	var role string
	var owner int32
	if err := e.pool.QueryRow(context.Background(), `SELECT m.role, w.owner_id FROM workspace_members m JOIN workspaces w ON w.id=m.workspace_id
		WHERE m.workspace_id=$1 AND m.user_id=$2`, id, uid).Scan(&role, &owner); err != nil {
		t.Fatal(err)
	}
	if role != "owner" || owner != uid {
		t.Fatalf("%s %d", role, owner)
	}
	if e.active(t, uid) != id {
		t.Fatal("new workspace is not active")
	}
}

func TestCreateSlugCollision409(t *testing.T) {
	e := newWsEnv(t)
	uid, hdr := e.user(t, "t11-dup")
	n := uniq(t)
	e.mk(t, n+"-x", "Taken", n+"-slug", uid, 1788000000, map[int32]string{uid: "owner"})
	e.setActive(t, uid, n+"-x")
	rec, _ := e.do("POST", "/api/workspaces", "application/json", mustJSON(map[string]string{"name": n + " Slug"}), hdr)
	if rec.Code != 409 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Workspace slug already exists"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if e.active(t, uid) != n+"-x" {
		t.Fatal("failed create changed active workspace")
	}
}

func TestSwitchOKAndForeignWorkspace403(t *testing.T) {
	e := newWsEnv(t)
	uid, hdr := e.user(t, "t11-sw")
	oid, _ := e.user(t, "t11-sw-o")
	n := uniq(t)
	e.mk(t, n+"-mine", "Mine", n+"-mine", uid, 1788000000, map[int32]string{uid: "member"})
	e.mk(t, n+"-theirs", "Theirs", n+"-theirs", oid, 1788000000, map[int32]string{oid: "owner"})
	rec, _ := e.do("POST", "/api/workspaces/switch", "application/json", mustJSON(map[string]string{"workspace_id": n + "-mine"}), hdr)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"active":"Mine","id":"`+n+`-mine"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	fx := contract.Load(t, "workspaces/switch_foreign")
	for _, id := range []string{n + "-theirs", n + "-missing"} {
		_, got := e.do("POST", "/api/workspaces/switch", "application/json", mustJSON(map[string]string{"workspace_id": id}), hdr)
		contract.AssertEqual(t, got, fx.Response)
	}
	if e.active(t, uid) != n+"-mine" {
		t.Fatal("active changed on denied switch")
	}
	fx = contract.Load(t, "workspaces/switch_missing_field")
	_, got := e.do("POST", "/api/workspaces/switch", "application/json", `{}`, hdr)
	contract.AssertEqual(t, got, fx.Response)
}

func TestSwitchAsOwnerWithoutMemberRow(t *testing.T) {
	e := newWsEnv(t)
	uid, hdr := e.user(t, "t11-swo")
	n := uniq(t)
	e.mk(t, n+"-o", "OwnedOnly", n+"-o", uid, 1788000000, nil)
	rec, _ := e.do("POST", "/api/workspaces/switch", "application/json", mustJSON(map[string]string{"workspace_id": n + "-o"}), hdr)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestDashboardRouteNotCapturedByWildcard(t *testing.T) {
	e := newWsEnv(t)
	uid, hdr := e.user(t, "t11-dash")
	n := uniq(t)
	e.mk(t, n+"-a", "Alpha", n+"-a", uid, 1788000000, map[int32]string{uid: "owner"})
	e.mk(t, n+"-b", "Beta", n+"-b", uid, 1788000001, map[int32]string{uid: "owner"})
	e.addLeads(t, n+"-a", 2)
	e.addLeads(t, n+"-b", 1)
	rec, _ := e.do("GET", "/api/workspaces/dashboard", "", "", hdr)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec.Body.Bytes())
	if m["total_workspaces"] != float64(2) || m["total_leads"] != float64(3) {
		t.Fatalf("%v", m)
	}
	fx := contract.Load(t, "workspaces/dashboard_alice")
	var fb struct{ Workspaces []json.RawMessage }
	_ = json.Unmarshal(fx.Response.Body, &fb)
	var rawDash struct{ Workspaces []json.RawMessage }
	_ = json.Unmarshal(rec.Body.Bytes(), &rawDash)
	first := rawDash.Workspaces[0]
	// leads_count differs on purpose (2 vs fixture 0); everything else matches.
	contract.AssertEqual(t, contract.Resp{Status: 200, Headers: fx.Response.Headers, Body: first},
		contract.Resp{Status: 200, Headers: fx.Response.Headers, Body: fb.Workspaces[0]},
		contract.IgnorePaths("$.id", "$.slug", "$.leads_count"))
	var topKeys []string
	for k := range m {
		topKeys = append(topKeys, k)
	}
	if len(topKeys) != 3 {
		t.Fatalf("%v", topKeys)
	}
	// "/switch" with GET must not be captured as ws_id either: 405-style, not a workspace lookup.
	rec, _ = e.do("GET", "/api/workspaces/switch", "", "", hdr)
	if rec.Code == 200 {
		t.Fatalf("GET /switch captured by wildcard: %s", rec.Body.String())
	}
}

func TestGetMemberAndNonMember404(t *testing.T) {
	e := newWsEnv(t)
	uid, hdr := e.user(t, "t11-get")
	oid, _ := e.user(t, "t11-get-o")
	n := uniq(t)
	e.mk(t, n+"-mine", "Mine", n+"-mine", oid, 1788000000, map[int32]string{oid: "owner", uid: "member"})
	e.mk(t, n+"-theirs", "Theirs", n+"-theirs", oid, 1788000000, map[int32]string{oid: "owner"})
	rec, got := e.do("GET", "/api/workspaces/"+n+"-mine", "", "", hdr)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	fx := contract.Load(t, "workspaces/get_member")
	contract.AssertEqual(t, got, fx.Response, contract.IgnorePaths("$.id", "$.slug", "$.owner_id", "$.name"))
	fx = contract.Load(t, "workspaces/get_non_member")
	for _, id := range []string{n + "-theirs", n + "-nope"} {
		_, got = e.do("GET", "/api/workspaces/"+id, "", "", hdr)
		contract.AssertEqual(t, got, fx.Response)
	}
	fxm := contract.Load(t, "workspaces/get_missing")
	if fxm.Response.Status != fx.Response.Status {
		t.Fatal("fixture drift")
	}
}

func TestDeleteNonOwner403(t *testing.T) {
	e := newWsEnv(t)
	uid, hdr := e.user(t, "t11-dno")
	oid, _ := e.user(t, "t11-dno-o")
	n := uniq(t)
	e.mk(t, n+"-w", "W", n+"-w", oid, 1788000000, map[int32]string{oid: "owner", uid: "admin"})
	fx := contract.Load(t, "workspaces/delete_non_owner")
	_, got := e.do("DELETE", "/api/workspaces/"+n+"-w", "", "", hdr)
	contract.AssertEqual(t, got, fx.Response)
	if !e.exists(t, n+"-w") {
		t.Fatal("workspace deleted by non-owner")
	}
}

func TestDeleteNotFoundForNonMemberAndMissing(t *testing.T) {
	e := newWsEnv(t)
	_, hdr := e.user(t, "t11-dnf")
	oid, _ := e.user(t, "t11-dnf-o")
	n := uniq(t)
	e.mk(t, n+"-w", "W", n+"-w", oid, 1788000000, map[int32]string{oid: "owner"})
	fx := contract.Load(t, "workspaces/delete_non_member")
	for _, id := range []string{n + "-w", n + "-none"} {
		_, got := e.do("DELETE", "/api/workspaces/"+id, "", "", hdr)
		contract.AssertEqual(t, got, fx.Response)
	}
	if !e.exists(t, n+"-w") {
		t.Fatal("workspace deleted by non-member")
	}
}

func TestDeleteMainIs400(t *testing.T) {
	e := newWsEnv(t)
	uid, hdr := e.user(t, "t11-dm")
	// A workspace whose slug is "main" may already exist; reuse it only if we can own it, else skip.
	var id string
	err := e.pool.QueryRow(context.Background(), `SELECT id FROM workspaces WHERE slug='main'`).Scan(&id)
	if err == nil {
		t.Skip("a 'main' workspace already exists in the shared test DB; not touching it")
	}
	id = uniq(t) + "-main"
	e.mk(t, id, "Default Workspace", "main", uid, 1788000000, map[int32]string{uid: "owner"})
	fx := contract.Load(t, "workspaces/delete_main")
	_, got := e.do("DELETE", "/api/workspaces/"+id, "", "", hdr)
	contract.AssertEqual(t, got, fx.Response)
	if !e.exists(t, id) {
		t.Fatal("main deleted")
	}
}

func TestDeleteOKMatchesFixtureAndClearsIdentityRows(t *testing.T) {
	e := newWsEnv(t)
	uid, hdr := e.user(t, "t11-dok")
	mid, _ := e.user(t, "t11-dok-m")
	n := uniq(t)
	e.mk(t, n+"-w", "W", n+"-w", uid, 1788000000, map[int32]string{uid: "owner", mid: "member"})
	e.setActive(t, mid, n+"-w")
	fx := contract.Load(t, "workspaces/delete_ok")
	_, got := e.do("DELETE", "/api/workspaces/"+n+"-w", "", "", hdr)
	contract.AssertEqual(t, got, fx.Response)
	if e.exists(t, n+"-w") || e.active(t, mid) != "" {
		t.Fatal("workspace or active pointer remains")
	}
	var members int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM workspace_members WHERE workspace_id=$1`, n+"-w").Scan(&members)
	if members != 0 {
		t.Fatal("members remain")
	}
	fx = contract.Load(t, "workspaces/get_after_delete")
	_, got = e.do("GET", "/api/workspaces/"+n+"-w", "", "", hdr)
	contract.AssertEqual(t, got, fx.Response)
}

func TestDeleteRemovesChildrenUnderRLS(t *testing.T) {
	e := newWsEnv(t)
	uid, hdr := e.user(t, "t11-drls")
	n := uniq(t)
	w, keep := n+"-w", n+"-keep"
	e.mk(t, w, "W", n+"-w", uid, 1788000000, map[int32]string{uid: "owner"})
	e.mk(t, keep, "Keep", n+"-keep", uid, 1788000000, map[int32]string{uid: "owner"})
	ctx := context.Background()
	for _, id := range []string{w, keep} {
		err := tenancy.InWorkspace(ctx, e.pool, id, func(tx pgx.Tx) error {
			for _, q := range []string{
				`INSERT INTO workspace_settings (workspace_id,key,value) VALUES ($1,'k','v')`,
				`INSERT INTO workspace_member_permissions (workspace_id,user_id,permission,effect,updated_at) VALUES ($1,` + fmt.Sprint(uid) + `,'p','allow',1)`,
				`INSERT INTO workspace_scim_groups (id,workspace_id,display_name,created_at,updated_at) VALUES ('g-` + id + `',$1,'G',1,1)`,
			} {
				if _, err := tx.Exec(ctx, q, id); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	rec, _ := e.do("DELETE", "/api/workspaces/"+w, "", "", hdr)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	count := func(id, tbl string) int {
		var c int
		_ = tenancy.InWorkspace(ctx, e.pool, id, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT count(*) FROM "+tbl).Scan(&c)
		})
		return c
	}
	for _, tbl := range []string{"workspace_settings", "workspace_member_permissions", "workspace_scim_groups"} {
		if c := count(w, tbl); c != 0 {
			t.Fatalf("%s: %d rows remain for deleted workspace", tbl, c)
		}
		if c := count(keep, tbl); c != 1 {
			t.Fatalf("%s: sibling workspace lost rows (%d)", tbl, c)
		}
	}
}

func TestDeleteRollsBackOnFailure(t *testing.T) {
	e := newWsEnv(t)
	uid, hdr := e.user(t, "t11-drb")
	n := uniq(t)
	w := n + "-w"
	e.mk(t, w, "W", n+"-w", uid, 1788000000, map[int32]string{uid: "owner"})
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, e.pool, w, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workspace_settings (workspace_id,key,value) VALUES ($1,'k','v')`, w)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// A trigger (installed as the schema owner) makes a late delete step fail.
	owner, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	fn := "t11_fail_" + strings.ReplaceAll(n, "-", "_")
	for _, q := range []string{
		`CREATE FUNCTION ` + fn + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF OLD.workspace_id = '` + w + `' THEN RAISE EXCEPTION 'boom'; END IF; RETURN OLD; END $$`,
		`CREATE TRIGGER ` + fn + ` BEFORE DELETE ON workspaces FOR EACH ROW EXECUTE FUNCTION ` + fn + `()`,
	} {
		if q2 := strings.Replace(q, "OLD.workspace_id", "OLD.id", 1); true {
			if _, err := owner.Exec(ctx, q2); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() {
		_, _ = owner.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+fn+` ON workspaces`)
		_, _ = owner.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+fn+`()`)
	})
	rec, _ := e.do("DELETE", "/api/workspaces/"+w, "", "", hdr)
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var members, settings int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_members WHERE workspace_id=$1`, w).Scan(&members)
	_ = tenancy.InWorkspace(ctx, e.pool, w, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM workspace_settings`).Scan(&settings)
	})
	if members != 1 || settings != 1 || !e.exists(t, w) {
		t.Fatalf("partial delete: members=%d settings=%d", members, settings)
	}
}

func TestLeadsCountUsesTenantScope(t *testing.T) {
	e := newWsEnv(t)
	uid, hdr := e.user(t, "t11-lc")
	n := uniq(t)
	e.mk(t, n+"-a", "A", n+"-a", uid, 1788000000, map[int32]string{uid: "owner"})
	e.mk(t, n+"-b", "B", n+"-b", uid, 1788000001, map[int32]string{uid: "owner"})
	e.addLeads(t, n+"-a", 3)
	e.addLeads(t, n+"-b", 1)
	rec, _ := e.do("GET", "/api/workspaces", "", "", hdr)
	list := decodeMap(t, rec.Body.Bytes())["workspaces"].([]any)
	if list[0].(map[string]any)["leads_count"] != float64(3) || list[1].(map[string]any)["leads_count"] != float64(1) {
		t.Fatalf("%v", list)
	}
	rec, _ = e.do("GET", "/api/workspaces/"+n+"-a", "", "", hdr)
	if decodeMap(t, rec.Body.Bytes())["leads_count"] != float64(3) {
		t.Fatal(rec.Body.String())
	}
}

func TestInactiveUserIs400(t *testing.T) {
	e := newWsEnv(t)
	e.seed(t, "t11-inactive", pythonHash, false, "user", false)
	rec, _ := e.do("GET", "/api/workspaces", "", "", bearer("t11-inactive"))
	if rec.Code != 400 {
		t.Fatalf("%d", rec.Code)
	}
}

// wsEntry returns the idx-th workspace entry of a list response body.
func wsEntry(t *testing.T, body []byte, idx int) json.RawMessage {
	t.Helper()
	var b struct{ Workspaces []json.RawMessage }
	if err := json.Unmarshal(body, &b); err != nil || idx >= len(b.Workspaces) {
		t.Fatalf("workspace entry %d: %v: %s", idx, err, body)
	}
	return b.Workspaces[idx]
}

func TestListAliceBobMatchFixtures(t *testing.T) {
	e := newWsEnv(t)
	aid, ah := e.user(t, "t11-fx-alice")
	bid, bh := e.user(t, "t11-fx-bob")
	n := uniq(t)
	e.mk(t, n+"-alpha", "Alpha", n+"-alpha", aid, 1788000000, map[int32]string{aid: "owner", bid: "member"})
	e.mk(t, n+"-beta", "Beta", n+"-beta", bid, 1788000000, map[int32]string{bid: "owner", aid: "member"})
	e.setActive(t, aid, n+"-alpha")
	e.setActive(t, bid, n+"-beta")
	ignore := contract.IgnorePaths("$.id", "$.slug", "$.owner_id")
	for _, c := range []struct {
		name string
		hdr  map[string]string
		want string
	}{{"workspaces/list_alice", ah, n + "-alpha"}, {"workspaces/list_bob", bh, n + "-beta"}} {
		fx := contract.Load(t, c.name)
		rec, got := e.do("GET", "/api/workspaces", "", "", c.hdr)
		if rec.Code != fx.Response.Status || decodeMap(t, rec.Body.Bytes())["active_id"] != c.want {
			t.Fatalf("%s: %d active_id=%v", c.name, rec.Code, decodeMap(t, rec.Body.Bytes())["active_id"])
		}
		for i := 0; i < 2; i++ { // alpha, beta; fixture alice also lists the seeded main workspace
			contract.AssertEqual(t,
				contract.Resp{Status: 200, Headers: got.Headers, Body: wsEntry(t, rec.Body.Bytes(), i)},
				contract.Resp{Status: 200, Headers: fx.Response.Headers, Body: wsEntry(t, fx.Response.Body, i)}, ignore)
		}
	}
}

func TestSwitchOKMatchesFixture(t *testing.T) {
	e := newWsEnv(t)
	uid, hdr := e.user(t, "t11-fx-sw")
	n := uniq(t)
	e.mk(t, n+"-alpha", "Alpha", n+"-alpha", uid, 1788000000, map[int32]string{uid: "owner"})
	fx := contract.Load(t, "workspaces/switch_ok")
	_, got := e.do("POST", "/api/workspaces/switch", "application/json", mustJSON(map[string]string{"workspace_id": n + "-alpha"}), hdr)
	contract.AssertEqual(t, got, fx.Response, contract.IgnorePaths("$.id"))
}
