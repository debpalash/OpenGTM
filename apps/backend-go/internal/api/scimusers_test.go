//go:build integration

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const patchOp = "urn:ietf:params:scim:api:messages:2.0:PatchOp"

type scimEnv struct {
	env
	pool *pgxpool.Pool
}

func newSCIMEnv(t *testing.T) scimEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 4)
	st := identity.NewStore(pool)
	// POST /Users inserts the users row before the workspace-scoped checks, so a
	// 409 (externalId collision) leaves an unlinked user behind that the
	// per-workspace cleanup cannot find. NewAppPool's cross-package lock is held
	// for the whole test, so any users row outside this baseline was created by it.
	baseline := []int32{}
	if err := pool.QueryRow(context.Background(), `SELECT COALESCE(array_agg(id), '{}') FROM users`).Scan(&baseline); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		rows, err := pool.Query(c, `SELECT id FROM users WHERE id <> ALL($1::int[])`, baseline)
		if err != nil {
			return
		}
		var ids []int32
		for rows.Next() {
			var id int32
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			_ = st.Delete(c, id)
		}
	})
	r := httpx.NewRouter()
	RegisterSCIM(r, Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{ScimTokenTTLDays: 90}})
	return scimEnv{env: env{h: r.Handler(), st: st}, pool: pool}
}

func scimHash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// workspace creates workspace id (slug == id) with a SCIM token and returns the bearer header.
// createdAt/expiresAt are epoch seconds; expiresAt 0 stores NULL.
func (e scimEnv) workspaceTok(t *testing.T, id string, createdAt, expiresAt float64) map[string]string {
	t.Helper()
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO workspaces (id, name, slug) VALUES ($1::varchar,$1::text,$1::text)`, id); err != nil {
		t.Fatal(err)
	}
	tok := "tok-" + id
	var exp any
	if expiresAt != 0 {
		exp = expiresAt
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO workspace_scim_tokens (workspace_id, token_hash, token_prefix, created_at, expires_at) VALUES ($1,$2,$3,$4,$5)`,
		id, scimHash(tok), tok[:8], createdAt, exp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		var ids []int32
		_ = tenancy.InWorkspace(c, e.pool, id, func(tx pgx.Tx) error {
			rows, err := tx.Query(c, `SELECT user_id FROM workspace_scim_users WHERE workspace_id=$1`, id)
			if err == nil {
				for rows.Next() {
					var u int32
					_ = rows.Scan(&u)
					ids = append(ids, u)
				}
				rows.Close()
			}
			_, _ = tx.Exec(c, `DELETE FROM workspace_scim_group_members WHERE workspace_id=$1`, id)
			_, _ = tx.Exec(c, `DELETE FROM workspace_scim_groups WHERE workspace_id=$1`, id)
			_, _ = tx.Exec(c, `DELETE FROM workspace_scim_users WHERE workspace_id=$1`, id)
			_, _ = tx.Exec(c, `DELETE FROM workspace_member_permissions WHERE workspace_id=$1`, id)
			return nil
		})
		_, _ = e.pool.Exec(c, `DELETE FROM workspace_oidc_identities WHERE workspace_id=$1`, id)
		_, _ = e.pool.Exec(c, `DELETE FROM workspace_scim_tokens WHERE workspace_id=$1`, id)
		_, _ = e.pool.Exec(c, `DELETE FROM workspace_members WHERE workspace_id=$1`, id)
		_, _ = e.pool.Exec(c, `DELETE FROM workspaces WHERE id=$1`, id)
		for _, u := range ids {
			_ = e.st.Delete(c, u)
		}
	})
	return map[string]string{"Authorization": "Bearer " + tok}
}

func (e scimEnv) workspace(t *testing.T, id string) map[string]string {
	t.Helper()
	return e.workspaceTok(t, id, float64(time.Now().Unix()), 0)
}

func (e scimEnv) call(method, path, body string, hdr map[string]string) (int, map[string]any, map[string]string) {
	ct := ""
	if body != "" {
		ct = "application/json"
	}
	rec, resp := e.do(method, path, ct, body, hdr)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m, resp.Headers
}

func (e scimEnv) create(t *testing.T, slug string, hdr map[string]string, body string) (int, map[string]any) {
	t.Helper()
	code, m, _ := e.call("POST", "/scim/v2/"+slug+"/Users", body, hdr)
	return code, m
}

func (e scimEnv) memberRole(t *testing.T, ws string, uid string) (string, bool) {
	t.Helper()
	var role string
	err := e.pool.QueryRow(context.Background(), `SELECT role FROM workspace_members WHERE workspace_id=$1 AND user_id=$2::int`, ws, uid).Scan(&role)
	if err == pgx.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return role, true
}

func (e scimEnv) mapping(t *testing.T, ws, uid string) (active bool, ok bool) {
	t.Helper()
	err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT active FROM workspace_scim_users WHERE workspace_id=$1 AND user_id=$2::int`, ws, uid).Scan(&active)
	})
	if err == pgx.ErrNoRows {
		return false, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return active, true
}

func TestBadSchemeOrExpiredTokenIs401(t *testing.T) {
	e := newSCIMEnv(t)
	now := float64(time.Now().Unix())
	good := e.workspace(t, "t13-auth")
	expired := e.workspaceTok(t, "t13-exp", now-100, now-1)
	oldNoExp := e.workspaceTok(t, "t13-old", now-91*86400, 0) // created_at + 90d ttl already passed
	cases := []struct {
		name, slug string
		hdr        map[string]string
	}{
		{"basic scheme", "t13-auth", map[string]string{"Authorization": "Basic " + strings.TrimPrefix(good["Authorization"], "Bearer ")}},
		{"wrong token", "t13-auth", map[string]string{"Authorization": "Bearer nope"}},
		{"empty token", "t13-auth", map[string]string{"Authorization": "Bearer "}},
		{"no header", "t13-auth", nil},
		{"expires_at passed", "t13-exp", expired},
		{"created_at+ttl passed", "t13-old", oldNoExp},
		{"token of another workspace", "t13-exp", good},
	}
	for _, c := range cases {
		code, m, h := e.call("GET", "/scim/v2/"+c.slug+"/Users", "", c.hdr)
		if code != 401 || m["detail"] != "Invalid SCIM bearer token" || h["WWW-Authenticate"] != "Bearer" {
			t.Errorf("%s: %d %v %v", c.name, code, m, h)
		}
	}
	// scheme match is case-insensitive
	if code, _, _ := e.call("GET", "/scim/v2/t13-auth/Users", "", map[string]string{"Authorization": "bEaReR " + strings.TrimPrefix(good["Authorization"], "Bearer ")}); code != 200 {
		t.Errorf("lowercase scheme: %d", code)
	}
}

func TestUnknownSlugIs404BeforeTokenCheck(t *testing.T) {
	e := newSCIMEnv(t)
	code, m, _ := e.call("GET", "/scim/v2/t13-nobody/Users", "", map[string]string{"Authorization": "Bearer junk"})
	if code != 404 || m["detail"] != "Workspace not found" {
		t.Fatalf("%d %v", code, m)
	}
}

func TestTokenLastUsedAtUpdatedAtMostOncePerMinute(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t13-touch")
	last := func() *float64 {
		var v *float64
		if err := e.pool.QueryRow(context.Background(), `SELECT last_used_at FROM workspace_scim_tokens WHERE workspace_id='t13-touch'`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if last() != nil {
		t.Fatal("last_used_at should start NULL")
	}
	e.call("GET", "/scim/v2/t13-touch/Users", "", hdr)
	first := last()
	if first == nil {
		t.Fatal("last_used_at not set")
	}
	e.call("GET", "/scim/v2/t13-touch/Users", "", hdr)
	if got := last(); got == nil || *got != *first {
		t.Fatalf("touched again within 60s: %v vs %v", got, *first)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE workspace_scim_tokens SET last_used_at = last_used_at - 61 WHERE workspace_id='t13-touch'`); err != nil {
		t.Fatal(err)
	}
	old := *last()
	e.call("GET", "/scim/v2/t13-touch/Users", "", hdr)
	if got := last(); got == nil || *got <= old+30 {
		t.Fatalf("not refreshed after 60s: %v old %v", got, old)
	}
}

func TestListFilterUserNameCaseInsensitive(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t13-flt")
	e.create(t, "t13-flt", hdr, `{"userName":"T13-Alice"}`)
	e.create(t, "t13-flt", hdr, `{"userName":"t13-bob"}`)
	for _, f := range []string{`userName eq "t13-ALICE"`, `  USERNAME   EQ  "T13-alice"  `} {
		code, m, _ := e.call("GET", "/scim/v2/t13-flt/Users?filter="+urlEnc(f), "", hdr)
		if code != 200 || m["totalResults"] != float64(1) || m["itemsPerPage"] != float64(1) {
			t.Fatalf("%q: %d %v", f, code, m)
		}
		if r := m["Resources"].([]any)[0].(map[string]any); r["userName"] != "t13-alice" {
			t.Fatalf("%v", r)
		}
	}
	code, m, _ := e.call("GET", "/scim/v2/t13-flt/Users?filter="+urlEnc(`userName eq "t13-ghost"`), "", hdr)
	if code != 200 || m["totalResults"] != float64(0) || len(m["Resources"].([]any)) != 0 {
		t.Fatalf("%d %v", code, m)
	}
	// startIndex beyond the single filtered hit yields no resources but total stays 1
	code, m, _ = e.call("GET", "/scim/v2/t13-flt/Users?startIndex=2&filter="+urlEnc(`userName eq "t13-bob"`), "", hdr)
	if code != 200 || m["totalResults"] != float64(1) || m["itemsPerPage"] != float64(0) {
		t.Fatalf("%d %v", code, m)
	}
}

func TestListFilterOtherAttributeIs400(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t13-flt4")
	for _, f := range []string{`displayName eq "x"`, `userName co "x"`, `userName eq x`, `userName eq ""`, `userName eq "a" and active eq true`} {
		code, m, _ := e.call("GET", "/scim/v2/t13-flt4/Users?filter="+urlEnc(f), "", hdr)
		if code != 400 || m["detail"] != "Only userName eq filtering is supported" {
			t.Errorf("%q: %d %v", f, code, m)
		}
	}
}

func TestListPaginationBounds(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t13-pg")
	for _, n := range []string{"t13-p1", "t13-p2", "t13-p3"} {
		e.create(t, "t13-pg", hdr, `{"userName":"`+n+`"}`)
	}
	for _, q := range []string{"startIndex=0", "startIndex=-1", "count=-1", "count=201", "count=x", "startIndex=1.5"} {
		code, m, _ := e.call("GET", "/scim/v2/t13-pg/Users?"+q, "", hdr)
		if code != 422 || m["detail"] == nil {
			t.Errorf("%s: %d %v", q, code, m)
		}
	}
	code, m, _ := e.call("GET", "/scim/v2/t13-pg/Users?count=0", "", hdr)
	if code != 200 || m["totalResults"] != float64(3) || m["itemsPerPage"] != float64(0) || m["startIndex"] != float64(1) {
		t.Fatalf("count=0: %d %v", code, m)
	}
	code, m, _ = e.call("GET", "/scim/v2/t13-pg/Users?startIndex=2&count=1", "", hdr)
	if code != 200 || m["totalResults"] != float64(3) || m["itemsPerPage"] != float64(1) || m["startIndex"] != float64(2) {
		t.Fatalf("page: %d %v", code, m)
	}
	if r := m["Resources"].([]any)[0].(map[string]any); r["userName"] != "t13-p2" {
		t.Fatalf("ordered by user id: %v", r)
	}
	code, m, _ = e.call("GET", "/scim/v2/t13-pg/Users?count=200&startIndex=4", "", hdr)
	if code != 200 || m["itemsPerPage"] != float64(0) || m["totalResults"] != float64(3) {
		t.Fatalf("past end: %d %v", code, m)
	}
}

func TestCreateLowercasesAndAddsViewer(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t13-cr")
	code, m := e.create(t, "t13-cr", hdr, `{"userName":" T13-Mixed.Case ","externalId":"x1","displayName":"Mixed"}`)
	if code != 201 {
		t.Fatalf("%d %v", code, m)
	}
	id := m["id"].(string)
	if m["userName"] != "t13-mixed.case" || m["active"] != true || m["externalId"] != "x1" || m["displayName"] != "Mixed" {
		t.Fatalf("%v", m)
	}
	if role, ok := e.memberRole(t, "t13-cr", id); !ok || role != "viewer" {
		t.Fatalf("membership %q %v", role, ok)
	}
	var hash string
	var active bool
	var role string
	if err := e.pool.QueryRow(context.Background(), `SELECT hashed_password, is_active, role FROM users WHERE id=$1::int`, id).Scan(&hash, &active, &role); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$2") || !active || role != "user" {
		t.Fatalf("user row hash=%q active=%v role=%q", hash[:min(len(hash), 4)], active, role)
	}
	if a, ok := e.mapping(t, "t13-cr", id); !ok || !a {
		t.Fatal("mapping missing")
	}
	// inactive create: mapping only, no membership; displayName falls back to username
	code, m = e.create(t, "t13-cr", hdr, `{"userName":"T13-Off","active":false}`)
	if code != 201 || m["active"] != false || m["displayName"] != "t13-off" {
		t.Fatalf("%d %v", code, m)
	}
	if _, ok := e.memberRole(t, "t13-cr", m["id"].(string)); ok {
		t.Fatal("inactive user must not become a member")
	}
}

func TestCreateReusesExistingUserRow(t *testing.T) {
	e := newSCIMEnv(t)
	hdrA := e.workspace(t, "t13-reuse-a")
	hdrB := e.workspace(t, "t13-reuse-b")
	_, a := e.create(t, "t13-reuse-a", hdrA, `{"userName":"t13-shared"}`)
	code, b := e.create(t, "t13-reuse-b", hdrB, `{"userName":"t13-shared"}`)
	if code != 201 || a["id"] != b["id"] {
		t.Fatalf("same users row expected: %d %v %v", code, a["id"], b["id"])
	}
}

func TestCreateDuplicateIs409(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t13-dup")
	e.create(t, "t13-dup", hdr, `{"userName":"t13-dupe","externalId":"e-1"}`)
	code, m := e.create(t, "t13-dup", hdr, `{"userName":"T13-DUPE"}`)
	if code != 409 || m["detail"] != "SCIM userName already exists" {
		t.Fatalf("%d %v", code, m)
	}
	code, m = e.create(t, "t13-dup", hdr, `{"userName":"t13-other","externalId":"e-1"}`)
	if code != 409 || m["detail"] != "SCIM externalId already exists" {
		t.Fatalf("externalId: %d %v", code, m)
	}
}

func TestCreateValidation422(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t13-val")
	for _, b := range []string{``, `not json`, `[]`, `{}`, `{"userName":""}`, `{"userName":1}`, `{"userName":"` + strings.Repeat("x", 151) + `"}`, `{"userName":"a","active":"maybe"}`, `{"userName":"a","externalId":null}`} {
		code, m := e.create(t, "t13-val", hdr, b)
		if code != 422 || m["detail"] == nil {
			t.Errorf("%.30q: %d %v", b, code, m)
		}
	}
	// lax booleans like pydantic
	code, m := e.create(t, "t13-val", hdr, `{"userName":"t13-lax","active":"false"}`)
	if code != 201 || m["active"] != false {
		t.Fatalf("%d %v", code, m)
	}
}

func (e scimEnv) patch(slug, id, ops string, hdr map[string]string) (int, map[string]any) {
	code, m, _ := e.call("PATCH", "/scim/v2/"+slug+"/Users/"+id, `{"schemas":["`+patchOp+`"],"Operations":`+ops+`}`, hdr)
	return code, m
}

func TestPatchDeactivateRemovesMembership(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t13-pd")
	_, u := e.create(t, "t13-pd", hdr, `{"userName":"t13-pd-u"}`)
	id := u["id"].(string)
	// capability overrides and OIDC identities go with the membership
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO workspace_oidc_identities (workspace_id, issuer, subject, user_id, created_at) VALUES ('t13-pd', 'iss', 'sub', $1::int, 0)`, id); err != nil {
		t.Fatal(err)
	}
	code, m := e.patch("t13-pd", id, `[{"op":"Replace","path":"active","value":false}]`, hdr)
	if code != 200 || m["active"] != false {
		t.Fatalf("%d %v", code, m)
	}
	if _, ok := e.memberRole(t, "t13-pd", id); ok {
		t.Fatal("membership must be removed")
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM workspace_oidc_identities WHERE workspace_id='t13-pd' AND user_id=$1::int`, id).Scan(&n)
	if n != 0 {
		t.Fatal("oidc identity must be removed with the membership")
	}
	if a, ok := e.mapping(t, "t13-pd", id); !ok || a {
		t.Fatal("mapping must stay, inactive")
	}
	// reactivate through a path-less value object; unknown keys are dropped
	code, m = e.patch("t13-pd", id, `[{"op":"add","value":{"active":true,"displayName":"Back","bogus":1}}]`, hdr)
	if code != 200 || m["active"] != true || m["displayName"] != "Back" {
		t.Fatalf("%d %v", code, m)
	}
	if role, ok := e.memberRole(t, "t13-pd", id); !ok || role != "viewer" {
		t.Fatalf("membership %q %v", role, ok)
	}
}

func TestPatchDeactivateNonMemberIsNoop(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t13-pn")
	_, u := e.create(t, "t13-pn", hdr, `{"userName":"t13-pn-u","active":false}`)
	code, m := e.patch("t13-pn", u["id"].(string), `[{"op":"replace","path":"active","value":false}]`, hdr)
	if code != 200 || m["active"] != false {
		t.Fatalf("%d %v", code, m)
	}
	if _, ok := e.memberRole(t, "t13-pn", u["id"].(string)); ok {
		t.Fatal("no membership expected")
	}
}

func TestPatchUnsupportedPathIs400(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t13-pu")
	_, u := e.create(t, "t13-pu", hdr, `{"userName":"t13-pu-u"}`)
	id := u["id"].(string)
	for _, c := range []struct{ ops, detail string }{
		{`[{"op":"replace","path":"emails","value":"x"}]`, "Unsupported patch path: emails"},
		{`[{"op":"replace","value":"x"}]`, "Unsupported patch path: None"},
		{`[{"op":"remove","path":"active"}]`, "Only add and replace patch operations are supported"},
		{`[{"path":"active","value":true}]`, "Only add and replace patch operations are supported"},
	} {
		code, m := e.patch("t13-pu", id, c.ops, hdr)
		if code != 400 || m["detail"] != c.detail {
			t.Errorf("%s: %d %v", c.ops, code, m)
		}
	}
	code, m, _ := e.call("PATCH", "/scim/v2/t13-pu/Users/"+id, `{"schemas":["x"],"Operations":[]}`, hdr)
	if code != 400 || m["detail"] != "PatchOp schema is required" {
		t.Fatalf("%d %v", code, m)
	}
	code, m = e.patch("t13-pu", id, `[{"op":"replace","path":"userName","value":"someone-else"}]`, hdr)
	if code != 409 || m["detail"] != "userName cannot be changed after provisioning" {
		t.Fatalf("%d %v", code, m)
	}
	// 404 precedes the schema check
	code, m, _ = e.call("PATCH", "/scim/v2/t13-pu/Users/999999", `{"schemas":["x"],"Operations":[]}`, hdr)
	if code != 404 || m["detail"] != "SCIM user not found" {
		t.Fatalf("%d %v", code, m)
	}
}

func TestPutUserNameChangeIs409(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t13-put")
	_, u := e.create(t, "t13-put", hdr, `{"userName":"t13-put-u","externalId":"a","displayName":"A"}`)
	id := u["id"].(string)
	code, m, _ := e.call("PUT", "/scim/v2/t13-put/Users/"+id, `{"userName":"t13-renamed"}`, hdr)
	if code != 409 || m["detail"] != "userName cannot be changed after provisioning" {
		t.Fatalf("%d %v", code, m)
	}
	// same name in another case is fine; omitted optional fields reset to their defaults
	code, m, _ = e.call("PUT", "/scim/v2/t13-put/Users/"+id, `{"userName":"T13-PUT-U","displayName":"B"}`, hdr)
	if code != 200 || m["displayName"] != "B" || m["externalId"] != "" || m["userName"] != "t13-put-u" || m["active"] != true {
		t.Fatalf("%d %v", code, m)
	}
	code, m, _ = e.call("PUT", "/scim/v2/t13-put/Users/999999", `{"userName":"x"}`, hdr)
	if code != 404 || m["detail"] != "SCIM user not found" {
		t.Fatalf("%d %v", code, m)
	}
}

func TestDeleteDeactivatesKeepsMapping(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t13-del")
	_, u := e.create(t, "t13-del", hdr, `{"userName":"t13-del-u","externalId":"keep"}`)
	id := u["id"].(string)
	rec, _ := e.do("DELETE", "/scim/v2/t13-del/Users/"+id, "", "", hdr)
	if rec.Code != 204 || rec.Body.Len() != 0 {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if _, ok := e.memberRole(t, "t13-del", id); ok {
		t.Fatal("membership must be removed")
	}
	if a, ok := e.mapping(t, "t13-del", id); !ok || a {
		t.Fatalf("mapping must stay, inactive: %v %v", a, ok)
	}
	code, m, _ := e.call("GET", "/scim/v2/t13-del/Users/"+id, "", hdr)
	if code != 200 || m["active"] != false || m["externalId"] != "keep" {
		t.Fatalf("%d %v", code, m)
	}
	// the users row survives
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE id=$1::int`, id).Scan(&n)
	if n != 1 {
		t.Fatal("users row must survive")
	}
	rec, _ = e.do("DELETE", "/scim/v2/t13-del/Users/999999", "", "", hdr)
	if rec.Code != 404 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestMappingsAreWorkspaceScoped(t *testing.T) {
	e := newSCIMEnv(t)
	hdrA := e.workspace(t, "t13-sc-a")
	hdrB := e.workspace(t, "t13-sc-b")
	_, u := e.create(t, "t13-sc-a", hdrA, `{"userName":"t13-scoped"}`)
	code, m, _ := e.call("GET", "/scim/v2/t13-sc-b/Users/"+u["id"].(string), "", hdrB)
	if code != 404 || m["detail"] != "SCIM user not found" {
		t.Fatalf("%d %v", code, m)
	}
	code, m, _ = e.call("GET", "/scim/v2/t13-sc-b/Users", "", hdrB)
	if code != 200 || m["totalResults"] != float64(0) {
		t.Fatalf("%d %v", code, m)
	}
	code, m, _ = e.call("GET", "/scim/v2/t13-sc-b/Users?filter="+urlEnc(`userName eq "t13-scoped"`), "", hdrB)
	if code != 200 || m["totalResults"] != float64(0) {
		t.Fatalf("filter: %d %v", code, m)
	}
}

func TestContentTypeScim(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t13-ct")
	_, u := e.create(t, "t13-ct", hdr, `{"userName":"t13-ct-u"}`)
	id := u["id"].(string)
	for _, p := range []string{"/ServiceProviderConfig", "/ResourceTypes", "/ResourceTypes/User", "/Schemas", "/Schemas/urn:ietf:params:scim:schemas:core:2.0:User", "/Users", "/Users/" + id} {
		_, _, h := e.call("GET", "/scim/v2/t13-ct"+p, "", hdr)
		if h["Content-Type"] != "application/scim+json" {
			t.Errorf("%s: %q", p, h["Content-Type"])
		}
	}
	_, _, h := e.call("POST", "/scim/v2/t13-ct/Users", `{"userName":"t13-ct-2"}`, hdr)
	if h["Content-Type"] != "application/scim+json" {
		t.Errorf("POST: %q", h["Content-Type"])
	}
	// errors are plain FastAPI HTTPException bodies
	_, _, h = e.call("GET", "/scim/v2/t13-ct/Users/999999", "", hdr)
	if h["Content-Type"] != "application/json" {
		t.Errorf("error: %q", h["Content-Type"])
	}
}

var (
	scimIDRe   = regexp.MustCompile(`/Users/(\d+)`)
	scimCaseRe = regexp.MustCompile(`^/scim/v2/(alpha|beta|nope)`)
)

// normalizeSCIM drops generated values (user ids, timestamps, locations) from user resources.
func normalizeSCIM(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			if _, isUser := t["userName"]; isUser {
				delete(t, "id")
				if meta, ok := t["meta"].(map[string]any); ok {
					delete(meta, "created")
					delete(meta, "lastModified")
					delete(meta, "location")
				}
			}
			for _, c := range t {
				walk(c)
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		}
	}
	walk(v)
	out, _ := json.Marshal(v)
	return out
}

func TestUsersMatchFixtures(t *testing.T) {
	e := newSCIMEnv(t)
	good := e.workspace(t, "t13-fx")
	expired := e.workspaceTok(t, "t13-fx-exp", float64(time.Now().Unix())-100, float64(time.Now().Unix())-1)
	goodTok := strings.TrimPrefix(good["Authorization"], "Bearer ")
	names := []string{
		"spc", "resource_types", "resource_type_user", "resource_type_missing", "schemas", "schema_group", "schema_missing",
		"unknown_slug", "no_auth", "bad_scheme", "bad_token", "expired_token", "list_empty",
		"create_ok", "create_dup", "create_inactive", "create_extid_collision", "create_missing_username", "create_bad_active", "create_empty_username",
		"list_all", "list_page", "list_count_zero", "list_count_too_big", "list_start_zero", "list_filter", "list_filter_none", "list_filter_other",
		"get_ok", "get_missing", "get_bad_id",
		"patch_deactivate", "patch_reactivate_valueobj", "patch_inactive_stays_noop", "patch_no_schema", "patch_remove_op", "patch_bad_path",
		"patch_username_change", "patch_username_same_case", "patch_missing_operations", "patch_missing_user",
		"put_ok", "put_username_change", "put_missing_user", "delete_ok", "get_after_delete", "delete_missing",
	}
	idMap := map[string]string{} // recorded user id -> actual
	for _, name := range names {
		fx := contract.Load(t, "scim_users/"+name)
		path := scimCaseRe.ReplaceAllStringFunc(fx.Request.Path, func(m string) string {
			switch strings.TrimPrefix(m, "/scim/v2/") {
			case "alpha":
				return "/scim/v2/t13-fx"
			case "beta":
				return "/scim/v2/t13-fx-exp"
			}
			return m
		})
		path = scimIDRe.ReplaceAllStringFunc(path, func(m string) string {
			if actual, ok := idMap[strings.TrimPrefix(m, "/Users/")]; ok {
				return "/Users/" + actual
			}
			return m
		})
		var hdr map[string]string
		switch name {
		case "no_auth":
		case "bad_scheme":
			hdr = map[string]string{"Authorization": "Basic " + goodTok}
		case "bad_token":
			hdr = map[string]string{"Authorization": "Bearer wrong"}
		case "unknown_slug":
			hdr = map[string]string{"Authorization": "Bearer whatever"}
		case "expired_token":
			hdr = expired
		default:
			hdr = good
		}
		ct, body := "", ""
		if len(fx.Request.Body) > 0 && string(fx.Request.Body) != "null" {
			ct, body = "application/json", string(fx.Request.Body)
		}
		_, got := e.do(fx.Request.Method, path, ct, body, hdr)
		if fx.Response.Status == 201 {
			var rec, act struct{ ID string }
			_ = json.Unmarshal(fx.Response.Body, &rec)
			_ = json.Unmarshal(got.Body, &act)
			if rec.ID != "" && act.ID != "" {
				idMap[rec.ID] = act.ID
			}
		}
		want := fx.Response
		want.Body = json.RawMessage(strings.ReplaceAll(string(want.Body), "/scim/v2/alpha/", "/scim/v2/t13-fx/"))
		got.Body, want.Body = normalizeSCIM(got.Body), normalizeSCIM(want.Body)
		t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, want) })
	}
}

func urlEnc(s string) string { return url.QueryEscape(s) }
