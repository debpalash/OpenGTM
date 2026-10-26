//go:build integration

package api

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

func (e scimEnv) group(t *testing.T, slug string, hdr map[string]string, body string) (int, map[string]any) {
	t.Helper()
	code, m, _ := e.call("POST", "/scim/v2/"+slug+"/Groups", body, hdr)
	return code, m
}

func (e scimEnv) uid(t *testing.T, slug string, hdr map[string]string, name string) string {
	t.Helper()
	code, m := e.create(t, slug, hdr, `{"userName":"`+name+`"}`)
	if code != 201 {
		t.Fatalf("create user %s: %d %v", name, code, m)
	}
	return m["id"].(string)
}

func gid(t *testing.T, m map[string]any) string {
	t.Helper()
	id, ok := m["id"].(string)
	if !ok {
		t.Fatalf("no group id in %v", m)
	}
	return id
}

func memberValues(m map[string]any) []string {
	out := []string{}
	list, _ := m["members"].([]any)
	for _, x := range list {
		out = append(out, x.(map[string]any)["value"].(string))
	}
	return out
}

func sameSet(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}

func patchBody(ops string) string {
	return `{"schemas":["` + patchOp + `"],"Operations":[` + ops + `]}`
}

func TestCreateGroupValidatesMembers(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t14-val")
	u1, u2 := e.uid(t, "t14-val", hdr, "t14-val-1"), e.uid(t, "t14-val", hdr, "t14-val-2")
	// unknown SCIM user -> 400, and no group row is left behind
	code, m := e.group(t, "t14-val", hdr, `{"displayName":"Ghosts","members":[{"value":"99999"}]}`)
	if code != 400 || m["detail"] != "SCIM user 99999 not found" {
		t.Fatalf("%d %v", code, m)
	}
	if c, m, _ := e.call("GET", "/scim/v2/t14-val/Groups", "", hdr); c != 200 || m["totalResults"] != float64(0) {
		t.Fatalf("group leaked: %d %v", c, m)
	}
	// duplicates collapse, order by user id, displayName trimmed, ids are uuids
	code, m = e.group(t, "t14-val", hdr, `{"displayName":"  Eng ","externalId":"x1","members":[{"value":"`+u2+`"},{"value":"`+u1+`"},{"value":`+u2+`}]}`)
	if code != 201 || m["displayName"] != "Eng" || m["externalId"] != "x1" {
		t.Fatalf("%d %v", code, m)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(m["id"].(string)) {
		t.Fatalf("id %v", m["id"])
	}
	if got := memberValues(m); !sameSet(got, []string{u1, u2}) {
		t.Fatalf("members %v want %v %v", got, u1, u2)
	}
	first := m["members"].([]any)[0].(map[string]any)
	if first["$ref"] != "/scim/v2/t14-val/Users/"+u1 {
		t.Fatalf("$ref %v", first["$ref"])
	}
	if sch := m["schemas"].([]any); len(sch) != 1 || sch[0] != "urn:ietf:params:scim:schemas:core:2.0:Group" {
		t.Fatalf("schemas %v", sch)
	}
	// a known user mapped but not a workspace member (inactive) is still a valid group member
	code, m = e.group(t, "t14-val", hdr, `{"displayName":"NoMembers"}`)
	if code != 201 || len(m["members"].([]any)) != 0 || m["externalId"] != "" {
		t.Fatalf("%d %v", code, m)
	}
}

func TestDuplicateDisplayName409(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t14-dup")
	if code, m := e.group(t, "t14-dup", hdr, `{"displayName":"Same"}`); code != 201 {
		t.Fatalf("%d %v", code, m)
	}
	code, m := e.group(t, "t14-dup", hdr, `{"displayName":" Same "}`)
	if code != 409 || m["detail"] != "SCIM group already exists" {
		t.Fatalf("%d %v", code, m)
	}
	// PUT/PATCH renaming onto an existing name also conflicts (deliberate: FastAPI answers 500)
	_, other := e.group(t, "t14-dup", hdr, `{"displayName":"Other"}`)
	id := gid(t, other)
	code, m, _ = e.call("PUT", "/scim/v2/t14-dup/Groups/"+id, `{"displayName":"Same"}`, hdr)
	if code != 409 || m["detail"] != "SCIM group already exists" {
		t.Fatalf("put %d %v", code, m)
	}
	code, m, _ = e.call("PATCH", "/scim/v2/t14-dup/Groups/"+id, patchBody(`{"op":"replace","path":"displayName","value":"Same"}`), hdr)
	if code != 409 || m["detail"] != "SCIM group already exists" {
		t.Fatalf("patch %d %v", code, m)
	}
	// the failed rename left the group intact
	if code, m, _ = e.call("GET", "/scim/v2/t14-dup/Groups/"+id, "", hdr); code != 200 || m["displayName"] != "Other" {
		t.Fatalf("%d %v", code, m)
	}
}

func TestNonNumericMember400(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t14-num")
	_, g := e.group(t, "t14-num", hdr, `{"displayName":"G"}`)
	id := gid(t, g)
	const want = "Each group member requires a numeric value"
	for _, members := range []string{`[{"value":"abc"}]`, `[{"display":"x"}]`, `[{"value":null}]`, `[{"value":[1]}]`, `["5"]`, `{"value":"1"}`} {
		code, m, _ := e.call("PATCH", "/scim/v2/t14-num/Groups/"+id, patchBody(`{"op":"add","path":"members","value":`+members+`}`), hdr)
		if code != 400 || m["detail"] != want {
			t.Errorf("patch %s: %d %v", members, code, m)
		}
	}
	code, m, _ := e.call("PUT", "/scim/v2/t14-num/Groups/"+id, `{"displayName":"G","members":[{"value":"x1"}]}`, hdr)
	if code != 400 || m["detail"] != want {
		t.Errorf("put: %d %v", code, m)
	}
	// FastAPI's create_group catches the HTTPException in its generic handler: POST answers 409.
	code, m = e.group(t, "t14-num", hdr, `{"displayName":"Fresh","members":[{"value":"abc"}]}`)
	if code != 409 || m["detail"] != "SCIM group already exists" {
		t.Errorf("post: %d %v", code, m)
	}
	// request validation: members must be a list of objects
	for _, body := range []string{`{"displayName":"V","members":["a"]}`, `{"displayName":"V","members":"a"}`, `{"displayName":""}`, `{"externalId":"x"}`} {
		if code, m = e.group(t, "t14-num", hdr, body); code != 422 || m["detail"] == nil {
			t.Errorf("%s: %d %v", body, code, m)
		}
	}
	// python int() leniency: numeric strings with padding and floats
	u := e.uid(t, "t14-num", hdr, "t14-num-u")
	code, m, _ = e.call("PUT", "/scim/v2/t14-num/Groups/"+id, `{"displayName":"G","members":[{"value":" `+u+` "}]}`, hdr)
	if code != 200 || !sameSet(memberValues(m), []string{u}) {
		t.Errorf("padded: %d %v", code, m)
	}
	code, m, _ = e.call("PUT", "/scim/v2/t14-num/Groups/"+id, `{"displayName":"G","members":[{"value":`+u+`.0}]}`, hdr)
	if code != 200 || !sameSet(memberValues(m), []string{u}) {
		t.Errorf("float: %d %v", code, m)
	}
}

func TestPatchAddRemoveMembers(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t14-patch")
	u1, u2, u3 := e.uid(t, "t14-patch", hdr, "t14-pa-1"), e.uid(t, "t14-patch", hdr, "t14-pa-2"), e.uid(t, "t14-patch", hdr, "t14-pa-3")
	_, g := e.group(t, "t14-patch", hdr, `{"displayName":"G","members":[{"value":"`+u1+`"}]}`)
	path := "/scim/v2/t14-patch/Groups/" + gid(t, g)

	code, m, _ := e.call("PATCH", path, patchBody(`{"op":"Add","path":"members","value":[{"value":"`+u2+`"},{"value":"`+u1+`"},{"value":"`+u3+`"}]}`), hdr)
	if code != 200 || !sameSet(memberValues(m), []string{u1, u2, u3}) {
		t.Fatalf("add: %d %v", code, m)
	}
	code, m, _ = e.call("PATCH", path, patchBody(`{"op":"remove","path":"members[value eq \"`+u2+`\"]"}`), hdr)
	if code != 200 || !sameSet(memberValues(m), []string{u1, u3}) {
		t.Fatalf("remove: %d %v", code, m)
	}
	// removing a non-member is a no-op; path match is case-insensitive
	code, m, _ = e.call("PATCH", path, patchBody(`{"op":"REMOVE","path":"MEMBERS[VALUE  EQ \"999\"]"}`), hdr)
	if code != 200 || !sameSet(memberValues(m), []string{u1, u3}) {
		t.Fatalf("remove noop: %d %v", code, m)
	}
	code, m, _ = e.call("PATCH", path, patchBody(`{"op":"replace","path":"members","value":[{"value":"`+u2+`"}]}`), hdr)
	if code != 200 || !sameSet(memberValues(m), []string{u2}) {
		t.Fatalf("replace: %d %v", code, m)
	}
	// falsy value empties membership on replace; add of nothing is a no-op
	code, m, _ = e.call("PATCH", path, patchBody(`{"op":"add","path":"members"}`), hdr)
	if code != 200 || !sameSet(memberValues(m), []string{u2}) {
		t.Fatalf("add none: %d %v", code, m)
	}
	code, m, _ = e.call("PATCH", path, patchBody(`{"op":"replace","path":"members","value":[]}`), hdr)
	if code != 200 || len(memberValues(m)) != 0 {
		t.Fatalf("replace empty: %d %v", code, m)
	}
	code, m, _ = e.call("PATCH", path, patchBody(`{"op":"replace","path":"displayName","value":"Renamed"},{"op":"add","path":"externalId","value":"ext"}`), hdr)
	if code != 200 || m["displayName"] != "Renamed" || m["externalId"] != "ext" {
		t.Fatalf("rename: %d %v", code, m)
	}
	// failures: a later bad op rolls back the whole patch
	for _, c := range []struct{ ops, detail string }{
		{`{"op":"remove","path":"members"}`, "Unsupported group remove path"},
		{`{"op":"remove","path":"members[value eq \"x\"]"}`, "Unsupported group remove path"},
		{`{"op":"add","path":"nope","value":1}`, "Unsupported group patch operation: add nope"},
		{`{"op":"Remove"}`, "Unsupported group patch operation: remove None"},
		{`{"op":"replace","path":"displayName","value":"Half"},{"op":"move","path":"x"}`, "Unsupported group patch operation: move x"},
		{`{"op":"add","path":"members","value":[{"value":"99999"}]}`, "SCIM user 99999 not found"},
	} {
		code, m, _ = e.call("PATCH", path, patchBody(c.ops), hdr)
		if code != 400 || m["detail"] != c.detail {
			t.Errorf("%s: %d %v", c.ops, code, m)
		}
	}
	if code, m, _ = e.call("GET", path, "", hdr); m["displayName"] != "Renamed" {
		t.Errorf("failed patch changed group: %d %v", code, m)
	}
	code, m, _ = e.call("PATCH", path, `{"schemas":["x"],"Operations":[]}`, hdr)
	if code != 400 || m["detail"] != "PatchOp schema is required" {
		t.Errorf("schema: %d %v", code, m)
	}
	code, m, _ = e.call("PATCH", "/scim/v2/t14-patch/Groups/nope", patchBody(``), hdr)
	if code != 404 || m["detail"] != "SCIM group not found" {
		t.Errorf("missing: %d %v", code, m)
	}
}

func TestPutReplacesMembers(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t14-put")
	u1, u2 := e.uid(t, "t14-put", hdr, "t14-put-1"), e.uid(t, "t14-put", hdr, "t14-put-2")
	_, g := e.group(t, "t14-put", hdr, `{"displayName":"G","externalId":"old","members":[{"value":"`+u1+`"}]}`)
	path := "/scim/v2/t14-put/Groups/" + gid(t, g)
	code, m, _ := e.call("PUT", path, `{"displayName":" G2 ","members":[{"value":"`+u2+`"}]}`, hdr)
	if code != 200 || m["displayName"] != "G2" || m["externalId"] != "" || !sameSet(memberValues(m), []string{u2}) {
		t.Fatalf("%d %v", code, m)
	}
	if m["meta"].(map[string]any)["created"] != g["meta"].(map[string]any)["created"] {
		t.Fatalf("created changed: %v vs %v", m["meta"], g["meta"])
	}
	// invalid member leaves the group untouched
	code, m, _ = e.call("PUT", path, `{"displayName":"G3","members":[{"value":"99999"}]}`, hdr)
	if code != 400 || m["detail"] != "SCIM user 99999 not found" {
		t.Fatalf("%d %v", code, m)
	}
	if _, m, _ = e.call("GET", path, "", hdr); m["displayName"] != "G2" || !sameSet(memberValues(m), []string{u2}) {
		t.Fatalf("rolled back: %v", m)
	}
	code, m, _ = e.call("PUT", "/scim/v2/t14-put/Groups/00000000-0000-0000-0000-000000000000", `{"displayName":"X"}`, hdr)
	if code != 404 || m["detail"] != "SCIM group not found" {
		t.Fatalf("%d %v", code, m)
	}
}

func TestDeleteGroupCascadesMembers(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t14-del")
	u1 := e.uid(t, "t14-del", hdr, "t14-del-1")
	_, g := e.group(t, "t14-del", hdr, `{"displayName":"G","members":[{"value":"`+u1+`"}]}`)
	id := gid(t, g)
	rec, _ := e.do("DELETE", "/scim/v2/t14-del/Groups/"+id, "", "", hdr)
	if rec.Code != 204 || rec.Body.Len() != 0 {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	var n int
	err := tenancy.InWorkspace(context.Background(), e.pool, "t14-del", func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM workspace_scim_group_members WHERE workspace_id='t14-del' AND group_id=$1`, id).Scan(&n)
	})
	if err != nil || n != 0 {
		t.Fatalf("members left: %d %v", n, err)
	}
	// the user mapping is untouched
	if _, ok := e.mapping(t, "t14-del", u1); !ok {
		t.Fatal("user mapping removed")
	}
	if code, m, _ := e.call("GET", "/scim/v2/t14-del/Groups/"+id, "", hdr); code != 404 || m["detail"] != "SCIM group not found" {
		t.Fatalf("%d %v", code, m)
	}
	if code, m, _ := e.call("DELETE", "/scim/v2/t14-del/Groups/"+id, "", hdr); code != 404 || m["detail"] != "SCIM group not found" {
		t.Fatalf("%d %v", code, m)
	}
}

func TestGroupListFilterAndPaging(t *testing.T) {
	e := newSCIMEnv(t)
	hdr := e.workspace(t, "t14-list")
	for _, n := range []string{"Charlie", "alpha", "Bravo"} {
		e.group(t, "t14-list", hdr, `{"displayName":"`+n+`"}`)
	}
	code, m, h := e.call("GET", "/scim/v2/t14-list/Groups", "", hdr)
	if code != 200 || m["totalResults"] != float64(3) || h["Content-Type"] != "application/scim+json" {
		t.Fatalf("%d %v %v", code, m, h)
	}
	code, m, _ = e.call("GET", "/scim/v2/t14-list/Groups?startIndex=2&count=1", "", hdr)
	if code != 200 || m["itemsPerPage"] != float64(1) || m["startIndex"] != float64(2) || m["totalResults"] != float64(3) {
		t.Fatalf("%d %v", code, m)
	}
	code, m, _ = e.call("GET", "/scim/v2/t14-list/Groups?filter="+urlEnc(`DisplayName EQ "ALPHA"`), "", hdr)
	if code != 200 || m["totalResults"] != float64(1) {
		t.Fatalf("%d %v", code, m)
	}
	code, m, _ = e.call("GET", "/scim/v2/t14-list/Groups?count=0", "", hdr)
	if code != 200 || m["totalResults"] != float64(3) || m["itemsPerPage"] != float64(0) {
		t.Fatalf("%d %v", code, m)
	}
	for _, f := range []string{`userName eq "x"`, `displayName eq ""`, `displayName co "a"`} {
		code, m, _ = e.call("GET", "/scim/v2/t14-list/Groups?filter="+urlEnc(f), "", hdr)
		if code != 400 || m["detail"] != "Only displayName eq filtering is supported" {
			t.Errorf("%q: %d %v", f, code, m)
		}
	}
	for _, q := range []string{"startIndex=0", "count=-1", "count=201"} {
		if code, _, _ = e.call("GET", "/scim/v2/t14-list/Groups?"+q, "", hdr); code != 422 {
			t.Errorf("%s: %d", q, code)
		}
	}
}

func TestGroupsIsolatedBetweenWorkspaces(t *testing.T) {
	e := newSCIMEnv(t)
	a, b := e.workspace(t, "t14-iso-a"), e.workspace(t, "t14-iso-b")
	ua := e.uid(t, "t14-iso-a", a, "t14-iso-user")
	_, ga := e.group(t, "t14-iso-a", a, `{"displayName":"Shared","members":[{"value":"`+ua+`"}]}`)
	id := gid(t, ga)
	// the same displayName is fine in another workspace
	if code, m := e.group(t, "t14-iso-b", b, `{"displayName":"Shared"}`); code != 201 {
		t.Fatalf("%d %v", code, m)
	}
	// user mapped only in workspace A is not a valid member in B
	code, m := e.group(t, "t14-iso-b", b, `{"displayName":"Other","members":[{"value":"`+ua+`"}]}`)
	if code != 400 || m["detail"] != "SCIM user "+ua+" not found" {
		t.Fatalf("%d %v", code, m)
	}
	// B cannot read, change or delete A's group, and A's token does not open B
	for _, c := range []struct{ method, body string }{{"GET", ""}, {"DELETE", ""}, {"PUT", `{"displayName":"x"}`}, {"PATCH", patchBody(``)}} {
		code, m, _ := e.call(c.method, "/scim/v2/t14-iso-b/Groups/"+id, c.body, b)
		if code != 404 || m["detail"] != "SCIM group not found" {
			t.Errorf("%s: %d %v", c.method, code, m)
		}
	}
	if code, _, _ := e.call("GET", "/scim/v2/t14-iso-b/Groups", "", a); code != 401 {
		t.Errorf("cross token: %d", code)
	}
	if code, m, _ := e.call("GET", "/scim/v2/t14-iso-b/Groups", "", b); code != 200 || m["totalResults"] != float64(1) {
		t.Errorf("b list: %d %v", code, m)
	}
	if code, m, _ := e.call("GET", "/scim/v2/t14-iso-a/Groups/"+id, "", a); code != 200 || len(memberValues(m)) != 1 {
		t.Errorf("a get: %d %v", code, m)
	}
}

var (
	scimUserValRe = regexp.MustCompile(`("value":\s*")(\d+)(")`)
	scimUserRefRe = regexp.MustCompile(`(/Users/)(\d+)()`)
	scimEqRe      = regexp.MustCompile(`(eq \\")(\d+)(\\")`)
	scimGroupMeta = regexp.MustCompile(`"(created|lastModified)":"[^"]*"`)
)

func TestGroupsMatchFixtures(t *testing.T) {
	e := newSCIMEnv(t)
	good := e.workspace(t, "t14-fx")
	names := []string{"user_g1", "user_g2", "user_g3", "list_empty", "create_ok", "create_no_members", "create_dup", "create_unknown_member", "create_nonnumeric_member", "create_missing_value", "create_missing_name", "create_empty_name", "list_all", "list_page", "list_count_zero", "list_count_too_big", "list_filter", "list_filter_none", "list_filter_other", "get_ok", "get_missing", "get_bad_id", "patch_add_members", "patch_remove_member", "patch_replace_members", "patch_rename", "patch_no_schema", "patch_bad_remove_path", "patch_bad_op", "patch_nonnumeric_member", "patch_unknown_member", "patch_missing", "put_ok", "put_unknown_member", "put_missing", "delete_ok", "get_after_delete", "delete_missing", "no_auth"}
	toActual := map[string]string{} // recorded id -> actual id (users and groups)
	toRecorded := map[string]string{}
	sub := func(re *regexp.Regexp, s string, m map[string]string) string {
		return re.ReplaceAllStringFunc(s, func(x string) string {
			p := re.FindStringSubmatch(x)
			if v, ok := m[p[2]]; ok {
				return p[1] + v + p[3]
			}
			return x
		})
	}
	ids := func(s string, m map[string]string) string {
		s = sub(scimUserValRe, s, m)
		s = sub(scimEqRe, s, m)
		s = sub(scimUserRefRe, s, m)
		for k, v := range m {
			if len(k) > 8 { // group uuids
				s = strings.ReplaceAll(s, k, v)
			}
		}
		return s
	}
	for _, name := range names {
		fx := contract.Load(t, "scim_groups/"+name)
		path := ids(strings.Replace(fx.Request.Path, "/scim/v2/alpha", "/scim/v2/t14-fx", 1), toActual)
		var hdr map[string]string
		if name != "no_auth" {
			hdr = good
		}
		ct, body := "", ""
		if len(fx.Request.Body) > 0 && string(fx.Request.Body) != "null" {
			ct, body = "application/json", ids(string(fx.Request.Body), toActual)
		}
		_, got := e.do(fx.Request.Method, path, ct, body, hdr)
		if fx.Response.Status == 201 {
			var rec, act struct{ ID string }
			_ = json.Unmarshal(fx.Response.Body, &rec)
			_ = json.Unmarshal(got.Body, &act)
			if rec.ID != "" && act.ID != "" {
				toActual[rec.ID], toRecorded[act.ID] = act.ID, rec.ID
			}
		}
		want := fx.Response
		strip := func(raw json.RawMessage) json.RawMessage {
			var v any
			if json.Unmarshal(raw, &v) != nil {
				return raw
			}
			out, _ := json.Marshal(v)
			return json.RawMessage(scimGroupMeta.ReplaceAllString(string(out), `"$1":"-"`))
		}
		want.Body = json.RawMessage(strings.ReplaceAll(string(want.Body), "/scim/v2/alpha/", "/scim/v2/t14-fx/"))
		got.Body = json.RawMessage(ids(string(strip(got.Body)), toRecorded))
		want.Body = json.RawMessage(ids(string(strip(want.Body)), nil))
		// user resources created in this file carry generated ids/timestamps
		got.Body, want.Body = normalizeSCIM(got.Body), normalizeSCIM(want.Body)
		t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, want) })
	}
}
