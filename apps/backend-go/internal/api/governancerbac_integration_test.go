//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/govadmin"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type rbEnv struct {
	mcpEnv
	r   *httpx.Router
	ids map[string]int32 // alice bob carol erin dave
}

// newRBEnv seeds the rbac fixture world (rbac_get_alpha / rbac_get_beta): alice owns ws-alpha, bob is a member of
// ws-alpha (tables.write allow, audiences.write deny) and owns ws-beta, erin is an editor of ws-alpha
// (activation.write deny). carol is a registered but inactive account that is a member of neither workspace.
type rbOpt int

const rbStaleGate rbOpt = 1

func newRBEnv(t *testing.T, opts ...rbOpt) rbEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Flags: config.Flags{PGLeadStore: true}}
	if len(opts) > 0 && opts[0] == rbStaleGate {
		// A gate decision taken before the caller lost the right: every authenticated workspace member passes.
		registerRBACRoutes(r, d, []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity)})
	} else {
		registerGovernanceRBAC(r, d)
	}
	// Probe route behind a catalog gate, to observe overrides through identity.RequireRole.
	r.Handle("GET /probe/tables", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { httpx.WriteJSON(w, 200, map[string]bool{"ok": true}) }),
		identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity), govadmin.Gate("tables.write"))
	e := rbEnv{mcpEnv: mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}, r: r, ids: map[string]int32{}}
	e.purge()
	for _, u := range []struct {
		n      string
		active bool
		admin  bool
	}{{"alice", true, true}, {"bob", true, false}, {"carol", false, false}, {"erin", true, false}, {"dave", true, false}} {
		role := "user"
		if u.admin {
			role = "admin"
		}
		e.seed(t, u.n, pythonHash, u.active, role, u.admin)
		e.ids[u.n] = e.mustUserByName(t, u.n).ID
	}
	e.workspace(t, "ws-alpha", e.ids["alice"])
	e.workspace(t, "ws-beta", e.ids["bob"])
	for _, m := range []struct {
		ws, who, role string
		at            float64
	}{{"ws-alpha", "alice", "owner", 1788000001}, {"ws-alpha", "bob", "member", 1788000002}, {"ws-alpha", "erin", "editor", 1788000004},
		{"ws-beta", "bob", "owner", 1788000000}, {"ws-beta", "alice", "owner", float64(time.Now().Unix())}} {
		if _, err := pool.Exec(context.Background(), `INSERT INTO workspace_members (workspace_id, user_id, role, created_at) VALUES ($1,$2,$3,$4)`, m.ws, e.ids[m.who], m.role, m.at); err != nil {
			t.Fatal(err)
		}
	}
	e.override(t, "ws-alpha", "bob", "tables.write", "allow")
	e.override(t, "ws-alpha", "bob", "audiences.write", "deny")
	e.override(t, "ws-alpha", "erin", "activation.write", "deny")
	t.Cleanup(e.purge)
	return e
}

func (e rbEnv) purge() {
	ctx := context.Background()
	for _, ws := range []string{"ws-alpha", "ws-beta"} {
		_ = tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			_, _ = tx.Exec(ctx, `DELETE FROM governance_audit_events`)
			_, _ = tx.Exec(ctx, `DELETE FROM workspace_member_permissions`)
			return nil
		})
	}
}

func (e rbEnv) override(t *testing.T, ws, who, perm, effect string) {
	t.Helper()
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workspace_member_permissions (workspace_id,user_id,permission,effect,updated_at) VALUES ($1,$2,$3,$4,1)
			ON CONFLICT (workspace_id,user_id,permission) DO UPDATE SET effect=EXCLUDED.effect`, ws, e.ids[who], perm, effect)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e rbEnv) roleOf(t *testing.T, ws, who string) string {
	t.Helper()
	var role string
	err := e.pool.QueryRow(context.Background(), `SELECT role FROM workspace_members WHERE workspace_id=$1 AND user_id=$2`, ws, e.ids[who]).Scan(&role)
	if err == pgx.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return role
}

func (e rbEnv) call(who, ws, method, path, body string) (*httptest.ResponseRecorder, contract.Resp) {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
		hdr["X-Workspace-Id"] = ws
	}
	ct := ""
	if body != "" {
		ct = "application/json"
	}
	return e.do(method, path, ct, body, hdr)
}

// fixtureIDs maps the recorded user ids 1..4 (alice bob carol erin) to this database's ids.
func (e rbEnv) fixtureID(n int) int32 {
	switch n {
	case 1:
		return e.ids["alice"]
	case 2:
		return e.ids["bob"]
	case 3:
		return e.ids["carol"]
	case 4:
		return e.ids["erin"]
	}
	return int32(n)
}

func (e rbEnv) remapPath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		if i > 0 && (parts[i-1] == "members" || parts[i-1] == "rbac") {
			if n, err := strconv.Atoi(s); err == nil {
				parts[i] = strconv.Itoa(int(e.fixtureID(n)))
			}
		}
	}
	return strings.Join(parts, "/")
}

func (e rbEnv) remapBody(raw json.RawMessage) json.RawMessage {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	var walk func(any) any
	walk = func(x any) any {
		switch m := x.(type) {
		case map[string]any:
			for k, val := range m {
				if n, ok := val.(json.Number); ok && k == "user_id" {
					if i, err := strconv.Atoi(string(n)); err == nil {
						m[k] = json.Number(strconv.Itoa(int(e.fixtureID(i))))
					}
					continue
				}
				m[k] = walk(val)
			}
		case []any:
			for i := range m {
				m[i] = walk(m[i])
			}
		}
		return x
	}
	out, _ := json.Marshal(walk(v))
	return out
}

// rbacSequence is the recorded call order (legacy/python/scripts/contract_cases/governance.yaml, area rbac); the
// fixtures are stateful, so they are replayed in this order against one world.
var rbacSequence = []struct{ name, who, ws string }{
	{"rbac_get_alpha", "alice", "ws-alpha"},
	{"rbac_get_beta", "bob", "ws-beta"},
	{"rbac_get_bob_403", "bob", "ws-alpha"},
	{"rbac_put_bob_403", "bob", "ws-alpha"},
	{"rbac_put_erin_403", "erin", "ws-alpha"},
	{"rbac_put_deny", "alice", "ws-alpha"},
	{"rbac_put_allow_outreach", "alice", "ws-alpha"},
	{"rbac_put_clear_null", "alice", "ws-alpha"},
	{"rbac_put_empty_body_clears", "alice", "ws-alpha"},
	{"rbac_put_invalid_effect_422", "alice", "ws-alpha"},
	{"rbac_put_unknown_permission_404", "alice", "ws-alpha"},
	{"rbac_put_unknown_permission_nonmember_404", "alice", "ws-alpha"},
	{"rbac_put_nonmember_404", "alice", "ws-alpha"},
	{"rbac_put_unknown_user_404", "alice", "ws-alpha"},
	{"rbac_put_owner_409", "alice", "ws-alpha"},
	{"rbac_put_user_not_int_422", "alice", "ws-alpha"},
	{"rbac_put_beta_owner_409", "bob", "ws-beta"},
	{"rbac_put_beta_foreign_member_404", "bob", "ws-beta"},
	{"rbac_get_after_puts", "alice", "ws-alpha"},
	{"members_add_bob_403", "bob", "ws-alpha"},
	{"members_add_unknown_user_404", "alice", "ws-alpha"},
	{"members_add_existing_409", "alice", "ws-alpha"},
	{"members_add_invalid_role_422", "alice", "ws-alpha"},
	{"members_add_empty_username_422", "alice", "ws-alpha"},
	{"members_add_username_too_long_422", "alice", "ws-alpha"},
	{"members_add_missing_username_422", "alice", "ws-alpha"},
	{"members_add_carol_201", "alice", "ws-alpha"},
	{"members_add_carol_again_409", "alice", "ws-alpha"},
	{"rbac_put_carol_deny", "alice", "ws-alpha"},
	{"members_patch_carol_admin", "alice", "ws-alpha"},
	{"members_patch_invalid_role_422", "alice", "ws-alpha"},
	{"members_patch_missing_role_422", "alice", "ws-alpha"},
	{"members_patch_nonmember_404", "alice", "ws-alpha"},
	{"members_patch_owner_409", "alice", "ws-alpha"},
	{"members_patch_erin_403", "erin", "ws-alpha"},
	{"members_delete_erin_403", "erin", "ws-alpha"},
	{"rbac_get_with_carol", "alice", "ws-alpha"},
	{"members_delete_owner_409", "alice", "ws-alpha"},
	{"members_delete_nonmember_404", "alice", "ws-alpha"},
	{"members_delete_carol_204", "alice", "ws-alpha"},
	{"members_delete_carol_again_404", "alice", "ws-alpha"},
	{"rbac_get_after_delete", "alice", "ws-alpha"},
}

func TestRbacRoutesMatchFixtures(t *testing.T) {
	e := newRBEnv(t)
	for _, step := range rbacSequence {
		fx := contract.Load(t, "rbac/"+step.name)
		body := ""
		if len(fx.Request.Body) > 0 && string(fx.Request.Body) != "null" {
			body = string(fx.Request.Body)
		}
		rec, got := e.call(step.who, step.ws, fx.Request.Method, e.remapPath(fx.Request.Path), body)
		want := fx.Response
		want.Body = e.remapBody(want.Body)
		var opts []contract.Option
		switch step.name {
		case "rbac_get_beta":
			opts = append(opts, contract.IgnorePaths("$.members[1].created_at"))
		case "rbac_get_with_carol":
			opts = append(opts, contract.IgnorePaths("$.members[3].created_at"))
		}
		if step.name == "members_delete_carol_204" {
			if rec.Body.Len() != 0 {
				t.Errorf("%s: 204 body %q", step.name, rec.Body.String())
			}
			if got.Status != 204 {
				t.Errorf("%s: status %d", step.name, got.Status)
			}
			continue
		}
		t.Run(step.name, func(t *testing.T) { contract.AssertEqual(t, got, want, opts...) })
	}
}

func TestDeleteMember204EmptyBody(t *testing.T) {
	e := newRBEnv(t)
	rec, _ := e.call("alice", "ws-alpha", "POST", "/api/governance/members", `{"username":"dave","role":"member"}`)
	if rec.Code != 201 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	rec, _ = e.call("alice", "ws-alpha", "DELETE", fmt.Sprintf("/api/governance/members/%d", e.ids["dave"]), "")
	if rec.Code != 204 || rec.Body.Len() != 0 {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Length") != "" && rec.Header().Get("Content-Length") != "0" {
		t.Errorf("content-length %q", rec.Header().Get("Content-Length"))
	}
	if e.roleOf(t, "ws-alpha", "dave") != "" {
		t.Error("member still present")
	}
}

func TestMemberRoutesGuards(t *testing.T) {
	e := newRBEnv(t)
	for _, c := range []struct {
		name, method, path, body string
		code                     int
		detail                   string
	}{
		{"owner role immutable", "PATCH", "/api/governance/members/%d", `{"role":"viewer"}`, 409, "Owner role cannot be changed"},
		{"owner not removable", "DELETE", "/api/governance/members/%d", ``, 409, "Workspace owner cannot be removed"},
		{"owner override", "PUT", "/api/governance/rbac/%d/tables.write", `{"effect":"deny"}`, 409, "Owner permissions cannot be overridden"},
	} {
		rec, _ := e.call("alice", "ws-alpha", c.method, fmt.Sprintf(c.path, e.ids["alice"]), c.body)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.detail) {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body.String())
		}
	}
	if e.roleOf(t, "ws-alpha", "alice") != "owner" {
		t.Fatal("owner was changed")
	}
	for _, c := range []struct {
		body   string
		code   int
		detail string
	}{{`{"username":"nobody"}`, 404, "User account not found"}, {`{"username":"bob"}`, 409, "User is already a workspace member"},
		{`{"username":"alice"}`, 409, "User is already a workspace member"}} {
		rec, _ := e.call("alice", "ws-alpha", "POST", "/api/governance/members", c.body)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.detail) {
			t.Errorf("%s: %d %s", c.body, rec.Code, rec.Body.String())
		}
	}
	// Add owner is not possible through the API at all.
	if rec, _ := e.call("alice", "ws-alpha", "POST", "/api/governance/members", `{"username":"dave","role":"owner"}`); rec.Code != 422 {
		t.Errorf("owner create: %d", rec.Code)
	}
	if e.roleOf(t, "ws-alpha", "dave") != "" {
		t.Error("dave added as owner")
	}
}

func TestMembersRequireAdmin403(t *testing.T) {
	e := newRBEnv(t)
	d := fmt.Sprint(e.ids["bob"])
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/governance/rbac", ""},
		{"PUT", "/api/governance/rbac/" + d + "/tables.write", `{"effect":"deny"}`},
		{"POST", "/api/governance/members", `{"username":"dave"}`},
		{"PATCH", "/api/governance/members/" + d, `{"role":"admin"}`},
		{"DELETE", "/api/governance/members/" + d, ""},
	} {
		for _, who := range []string{"bob", "erin"} {
			rec, _ := e.call(who, "ws-alpha", c.method, c.path, c.body)
			if rec.Code != 403 || !strings.Contains(rec.Body.String(), "Workspace permission denied: governance.manage") {
				t.Errorf("%s %s %s: %d %s", who, c.method, c.path, rec.Code, rec.Body.String())
			}
		}
		if rec, _ := e.call("", "", c.method, c.path, c.body); rec.Code != 401 {
			t.Errorf("anon %s %s: %d", c.method, c.path, rec.Code)
		}
	}
	if e.roleOf(t, "ws-alpha", "bob") != "member" || e.roleOf(t, "ws-alpha", "dave") != "" {
		t.Fatal("a denied request changed state")
	}
}

func TestOverrideAffectsRequireRoleImmediately(t *testing.T) {
	e := newRBEnv(t)
	probe := func(who string) int {
		rec, _ := e.call(who, "ws-alpha", "GET", "/probe/tables", "")
		return rec.Code
	}
	put := func(who, effect string) {
		t.Helper()
		body := `{"effect":null}`
		if effect != "" {
			body = `{"effect":"` + effect + `"}`
		}
		if rec, _ := e.call("alice", "ws-alpha", "PUT", fmt.Sprintf("/api/governance/rbac/%d/tables.write", e.ids[who]), body); rec.Code != 200 {
			t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
		}
	}
	if probe("bob") != 200 { // seeded allow
		t.Fatal("seeded allow not honoured")
	}
	put("bob", "")
	if probe("bob") != 403 { // member default has no tables.write
		t.Fatal("clearing the override did not take effect on the next request")
	}
	put("bob", "allow")
	if probe("bob") != 200 {
		t.Fatal("allow did not take effect on the next request")
	}
	if probe("erin") != 200 { // editor default
		t.Fatal("editor default")
	}
	put("erin", "deny")
	if probe("erin") != 403 {
		t.Fatal("deny did not take effect on the next request")
	}
	// A role change keeps overrides and re-evaluates immediately.
	if rec, _ := e.call("alice", "ws-alpha", "PATCH", fmt.Sprintf("/api/governance/members/%d", e.ids["erin"]), `{"role":"viewer"}`); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	put("erin", "")
	if probe("erin") != 403 {
		t.Fatal("viewer must not pass the tables.write gate")
	}
	// Removal takes effect on the next request too.
	if rec, _ := e.call("alice", "ws-alpha", "DELETE", fmt.Sprintf("/api/governance/members/%d", e.ids["bob"]), ""); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	if c := probe("bob"); c != 403 && c != 404 {
		t.Fatalf("removed member got %d", c)
	}
}

func (e rbEnv) lastAudit(t *testing.T, ws, method string) (status int, meta string, actor *int32, role string) {
	t.Helper()
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT response_status, metadata_json::text, actor_user_id, coalesce(actor_role,'') FROM governance_audit_events
			WHERE workspace_id=$1 AND method=$2 ORDER BY created_at DESC, id DESC LIMIT 1`, ws, method).Scan(&status, &meta, &actor, &role)
	})
	if err != nil {
		t.Fatalf("no audit event for %s: %v", method, err)
	}
	return
}

func TestMemberRoutesAuditMetadataHasNoSecrets(t *testing.T) {
	e := newRBEnv(t)
	secret := "s3cr3t-password-xyz"
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/governance/members", `{"username":"dave","role":"editor","password":"` + secret + `","api_token":"` + secret + `"}`},
		{"PATCH", fmt.Sprintf("/api/governance/members/%d", e.ids["dave"]), `{"role":"admin","client_secret":"` + secret + `"}`},
		{"PUT", fmt.Sprintf("/api/governance/rbac/%d/tables.write", e.ids["dave"]), `{"effect":"deny","token":"` + secret + `"}`},
		{"DELETE", fmt.Sprintf("/api/governance/members/%d", e.ids["dave"]), ``},
	} {
		if rec, _ := e.call("alice", "ws-alpha", c.method, c.path, c.body); rec.Code >= 300 {
			t.Fatalf("%s: %d %s", c.method, rec.Code, rec.Body.String())
		}
		status, meta, actor, role := e.lastAudit(t, "ws-alpha", c.method)
		if strings.Contains(meta, secret) || strings.Contains(meta, "dave") {
			t.Errorf("%s: audit metadata carries request data: %s", c.method, meta)
		}
		if strings.TrimSpace(meta) != "{}" { // Python records no audit_metadata for these routes
			t.Errorf("%s: metadata %s, want {}", c.method, meta)
		}
		if status < 200 || actor == nil || *actor != e.ids["alice"] || role != "owner" {
			t.Errorf("%s: audit actor %v role %q status %d", c.method, actor, role, status)
		}
	}
	// Denied writes are audited as well, still without request data.
	e.call("bob", "ws-alpha", "POST", "/api/governance/members", `{"username":"dave","password":"`+secret+`"}`)
	status, meta, _, _ := e.lastAudit(t, "ws-alpha", "POST")
	if status != 403 || strings.Contains(meta, secret) {
		t.Errorf("denied: %d %s", status, meta)
	}
}

func TestRbacTenantIsolation(t *testing.T) {
	e := newRBEnv(t)
	// ws-beta sees only its own members: bob and alice (owners), never alpha's erin or alpha's overrides.
	rec, _ := e.call("bob", "ws-beta", "GET", "/api/governance/rbac", "")
	var body struct {
		Members []struct {
			UserID    int32             `json:"user_id"`
			Overrides map[string]string `json:"overrides"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Members) != 2 {
		t.Fatalf("beta members: %v %s", err, rec.Body.String())
	}
	for _, m := range body.Members {
		if m.UserID == e.ids["erin"] || len(m.Overrides) != 0 {
			t.Errorf("alpha data visible in beta: %+v", m)
		}
	}
	// Foreign workspace the caller is not a member of: denied, not a 404 oracle for membership.
	if rec, _ := e.call("erin", "ws-beta", "GET", "/api/governance/rbac", ""); rec.Code != 403 && rec.Code != 404 {
		t.Errorf("erin in beta: %d", rec.Code)
	}
	// Beta admin acting on an alpha member id: 404, and alpha is untouched.
	erin := e.ids["erin"]
	for _, c := range []struct{ method, path, body string }{
		{"PATCH", fmt.Sprintf("/api/governance/members/%d", erin), `{"role":"viewer"}`},
		{"DELETE", fmt.Sprintf("/api/governance/members/%d", erin), ``},
		{"PUT", fmt.Sprintf("/api/governance/rbac/%d/tables.write", erin), `{"effect":"allow"}`},
	} {
		rec, _ := e.call("bob", "ws-beta", c.method, c.path, c.body)
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), "Workspace member not found") {
			t.Errorf("%s: %d %s", c.method, rec.Code, rec.Body.String())
		}
	}
	if e.roleOf(t, "ws-alpha", "erin") != "editor" {
		t.Fatal("alpha membership was changed from beta")
	}
	// Adding in beta does not add in alpha.
	if rec, _ := e.call("bob", "ws-beta", "POST", "/api/governance/members", `{"username":"dave"}`); rec.Code != 201 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if e.roleOf(t, "ws-alpha", "dave") != "" || e.roleOf(t, "ws-beta", "dave") != "viewer" {
		t.Fatal("member landed in the wrong workspace")
	}
}

// --- privilege escalation (hardening; Python only gates on governance.manage) --------------------------------

// grantManage gives erin (editor) governance.manage by override, the only way a non-admin reaches these routes.
func (e rbEnv) grantManage(t *testing.T) {
	t.Helper()
	e.override(t, "ws-alpha", "erin", "governance.manage", "allow")
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO workspace_members (workspace_id,user_id,role,created_at) VALUES ('ws-alpha',$1,'admin',1788000005)`, e.ids["dave"]); err != nil {
		t.Fatal(err)
	}
}

func TestMemberRoutesBlockPrivilegeEscalation(t *testing.T) {
	e := newRBEnv(t)
	e.grantManage(t) // erin: editor + governance.manage; dave: admin
	erin, dave, bob := e.ids["erin"], e.ids["dave"], e.ids["bob"]
	deny := func(name, method, path, body string) {
		t.Helper()
		rec, _ := e.call("erin", "ws-alpha", method, path, body)
		if rec.Code != 403 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	deny("grant admin on create", "POST", "/api/governance/members", `{"username":"carol","role":"admin"}`)
	deny("promote member to admin", "PATCH", fmt.Sprintf("/api/governance/members/%d", bob), `{"role":"admin"}`)
	deny("self promote to admin", "PATCH", fmt.Sprintf("/api/governance/members/%d", erin), `{"role":"admin"}`)
	deny("demote admin", "PATCH", fmt.Sprintf("/api/governance/members/%d", dave), `{"role":"viewer"}`)
	deny("remove admin", "DELETE", fmt.Sprintf("/api/governance/members/%d", dave), ``)
	deny("override admin", "PUT", fmt.Sprintf("/api/governance/rbac/%d/tables.write", dave), `{"effect":"deny"}`)
	deny("grant permission caller lacks", "PUT", fmt.Sprintf("/api/governance/rbac/%d/outreach.write", bob), `{"effect":"allow"}`)
	deny("self grant permission caller lacks", "PUT", fmt.Sprintf("/api/governance/rbac/%d/secrets.manage", erin), `{"effect":"allow"}`)
	if e.roleOf(t, "ws-alpha", "bob") != "member" || e.roleOf(t, "ws-alpha", "erin") != "editor" || e.roleOf(t, "ws-alpha", "dave") != "admin" || e.roleOf(t, "ws-alpha", "carol") != "" {
		t.Fatal("an escalation attempt changed state")
	}
	// Within their own level they can still work (no regression for delegated managers).
	for _, c := range []struct{ name, method, path, body string }{
		{"grant editor on create", "POST", "/api/governance/members", `{"username":"carol","role":"editor"}`},
		{"promote member to editor", "PATCH", fmt.Sprintf("/api/governance/members/%d", bob), `{"role":"editor"}`},
		{"grant permission caller holds", "PUT", fmt.Sprintf("/api/governance/rbac/%d/campaigns.write", bob), `{"effect":"allow"}`},
		{"deny is always allowed", "PUT", fmt.Sprintf("/api/governance/rbac/%d/outreach.write", bob), `{"effect":"deny"}`},
		{"clear", "PUT", fmt.Sprintf("/api/governance/rbac/%d/outreach.write", bob), `{"effect":null}`},
	} {
		if rec, _ := e.call("erin", "ws-alpha", c.method, c.path, c.body); rec.Code >= 300 {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body.String())
		}
	}
	// Owners and admins are unaffected: an admin may manage admins (Python parity).
	if rec, _ := e.call("alice", "ws-alpha", "PATCH", fmt.Sprintf("/api/governance/members/%d", dave), `{"role":"admin"}`); rec.Code != 200 {
		t.Errorf("owner patch admin: %d", rec.Code)
	}
}

func TestAdminSelfServiceMatchesPython(t *testing.T) {
	e := newRBEnv(t)
	e.grantManage(t)
	// Python lets an admin change their own role and remove themselves (only the owner is protected).
	dave := e.ids["dave"]
	if rec, _ := e.call("dave", "ws-alpha", "PATCH", fmt.Sprintf("/api/governance/members/%d", dave), `{"role":"viewer"}`); rec.Code != 200 {
		t.Fatalf("self demote: %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := e.call("dave", "ws-alpha", "GET", "/api/governance/rbac", ""); rec.Code != 403 {
		t.Fatalf("demoted admin still has access: %d", rec.Code)
	}
}

func TestLastOwnerCannotBeRemovedOrDemoted(t *testing.T) {
	e := newRBEnv(t)
	e.grantManage(t)
	alice := e.ids["alice"]
	// Even a real admin, and the owner acting on themselves, cannot remove or demote the owner.
	for _, who := range []string{"alice", "dave"} {
		for _, c := range []struct{ method, path, body string }{
			{"PATCH", fmt.Sprintf("/api/governance/members/%d", alice), `{"role":"admin"}`},
			{"DELETE", fmt.Sprintf("/api/governance/members/%d", alice), ``},
		} {
			if rec, _ := e.call(who, "ws-alpha", c.method, c.path, c.body); rec.Code != 409 {
				t.Errorf("%s %s: %d %s", who, c.method, rec.Code, rec.Body.String())
			}
		}
	}
	if e.roleOf(t, "ws-alpha", "alice") != "owner" {
		t.Fatal("owner changed")
	}
}

func TestMemberWritesRecheckPermissionInTx(t *testing.T) {
	e := newRBEnv(t, rbStaleGate)
	bob := e.ids["bob"]
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/governance/members", `{"username":"carol","role":"viewer"}`},
		{"PATCH", fmt.Sprintf("/api/governance/members/%d", bob), `{"role":"admin"}`},
		{"PUT", fmt.Sprintf("/api/governance/rbac/%d/tables.write", bob), `{"effect":"deny"}`},
		{"DELETE", fmt.Sprintf("/api/governance/members/%d", bob), ``},
	} {
		// erin is a plain editor: the stale gate admitted her, the in-transaction check must refuse.
		rec, _ := e.call("erin", "ws-alpha", c.method, c.path, c.body)
		if rec.Code != 403 {
			t.Errorf("%s: %d %s", c.method, rec.Code, rec.Body.String())
		}
	}
	if e.roleOf(t, "ws-alpha", "bob") != "member" || e.roleOf(t, "ws-alpha", "carol") != "" {
		t.Fatal("stale-gate request changed state")
	}
}

// A concurrent demotion of the acting admin must wait for (or precede) the member write: the write holds the
// actor's membership row until it commits.
func TestMemberWriteLocksActorRow(t *testing.T) {
	e := newRBEnv(t)
	e.grantManage(t)
	ctx := context.Background()
	dave, bob := e.ids["dave"], e.ids["bob"]
	ready, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
			if _, _, err := govadmin.Authorize(ctx, tx, "ws-alpha", dave, bob); err != nil {
				return err
			}
			close(ready)
			<-release
			return nil
		})
	}()
	select {
	case <-ready:
	case err := <-first:
		t.Fatalf("authorize failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("authorize hung")
	}
	done := make(chan error, 1)
	go func() {
		done <- tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
			_, err := govadmin.SetRole(ctx, tx, "ws-alpha", dave, "viewer")
			return err
		})
	}()
	select {
	case err := <-done:
		t.Fatalf("actor demotion did not wait for the in-flight write (err %v)", err)
	case <-time.After(400 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("demotion never completed")
	}
}
