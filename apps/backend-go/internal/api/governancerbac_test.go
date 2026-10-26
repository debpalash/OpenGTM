package api

import (
	"encoding/json"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

func rbacErrs(t *testing.T, errs []httpx.FieldError) string {
	t.Helper()
	b, err := json.Marshal(errs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const roleExpectedCtx = `"ctx":{"expected":"'viewer', 'member', 'editor' or 'admin'"}`

func TestRbacPutEffectInvalid422(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{`{"effect":"maybe"}`, `[{"type":"literal_error","loc":["body","effect"],"msg":"Input should be 'allow' or 'deny'","input":"maybe","ctx":{"expected":"'allow' or 'deny'"}}]`},
		{`{"effect":5}`, `[{"type":"literal_error","loc":["body","effect"],"msg":"Input should be 'allow' or 'deny'","input":5,"ctx":{"expected":"'allow' or 'deny'"}}]`},
		{`{"effect":"ALLOW"}`, `[{"type":"literal_error","loc":["body","effect"],"msg":"Input should be 'allow' or 'deny'","input":"ALLOW","ctx":{"expected":"'allow' or 'deny'"}}]`},
		{``, `[{"type":"missing","loc":["body"],"msg":"Field required","input":null}]`},
		{`[1]`, `[{"type":"model_attributes_type","loc":["body"],"msg":"Input should be a valid dictionary or object to extract fields from","input":[1]}]`},
		{`{`, `[{"type":"json_invalid","loc":["body",0],"msg":"JSON decode error","input":{}}]`},
	} {
		_, errs := parsePermissionUpdate([]byte(c.body))
		if got := rbacErrs(t, errs); got != c.want {
			t.Errorf("%q:\n got %s\nwant %s", c.body, got, c.want)
		}
	}
	for _, ok := range []string{`{}`, `{"effect":null}`, `{"effect":"allow","extra":1}`} {
		if _, errs := parsePermissionUpdate([]byte(ok)); errs != nil {
			t.Errorf("%q rejected: %s", ok, rbacErrs(t, errs))
		}
	}
	if e, _ := parsePermissionUpdate([]byte(`{"effect":"deny"}`)); e == nil || *e != "deny" {
		t.Errorf("deny: %v", e)
	}
	if e, _ := parsePermissionUpdate([]byte(`{"effect":null}`)); e != nil {
		t.Errorf("null: %v", e)
	}
}

func TestMemberRoleEnumRejectsOwner422(t *testing.T) {
	want := `[{"type":"literal_error","loc":["body","role"],"msg":"Input should be 'viewer', 'member', 'editor' or 'admin'","input":"owner",` + roleExpectedCtx + `}]`
	if _, errs := parseMemberRoleUpdate([]byte(`{"role":"owner"}`)); rbacErrs(t, errs) != want {
		t.Errorf("patch: %s", rbacErrs(t, errs))
	}
	if _, _, errs := parseMemberCreate([]byte(`{"username":"carol","role":"owner"}`)); rbacErrs(t, errs) != want {
		t.Errorf("create: %s", rbacErrs(t, errs))
	}
	for _, r := range []string{`"Admin"`, `" admin"`, `""`, `7`, `null`, `true`} {
		if _, errs := parseMemberRoleUpdate([]byte(`{"role":` + r + `}`)); len(errs) != 1 || errs[0].Type != "literal_error" {
			t.Errorf("role %s: %s", r, rbacErrs(t, errs))
		}
	}
	if _, errs := parseMemberRoleUpdate([]byte(`{}`)); rbacErrs(t, errs) != `[{"type":"missing","loc":["body","role"],"msg":"Field required","input":{}}]` {
		t.Errorf("missing: %s", rbacErrs(t, errs))
	}
	for _, r := range []string{"viewer", "member", "editor", "admin"} {
		if got, errs := parseMemberRoleUpdate([]byte(`{"role":"` + r + `"}`)); errs != nil || got != r {
			t.Errorf("%s: %v %s", r, got, rbacErrs(t, errs))
		}
	}
}

func TestMemberCreateBodyValidation(t *testing.T) {
	u, r, errs := parseMemberCreate([]byte(`{"username":" carol "}`))
	if errs != nil || u != " carol " || r != "viewer" {
		t.Fatalf("default role: %q %q %s", u, r, rbacErrs(t, errs))
	}
	long := make([]byte, 151)
	for i := range long {
		long[i] = 'u'
	}
	_, _, errs = parseMemberCreate([]byte(`{"username":"` + string(long) + `"}`))
	if len(errs) != 1 || errs[0].Type != "string_too_long" || errs[0].Ctx["max_length"] != 150 {
		t.Errorf("too long: %s", rbacErrs(t, errs))
	}
	// 150 code points of a 4-byte rune is still valid: pydantic counts characters.
	wide := ""
	for i := 0; i < 150; i++ {
		wide += "\U0001F600"
	}
	if _, _, errs = parseMemberCreate([]byte(`{"username":"` + wide + `"}`)); errs != nil {
		t.Errorf("150 runes rejected: %s", rbacErrs(t, errs))
	}
	if _, _, errs = parseMemberCreate([]byte(`{"username":""}`)); len(errs) != 1 || errs[0].Type != "string_too_short" || errs[0].Ctx["min_length"] != 1 {
		t.Errorf("empty: %s", rbacErrs(t, errs))
	}
	if _, _, errs = parseMemberCreate([]byte(`{"username":5}`)); len(errs) != 1 || errs[0].Type != "string_type" {
		t.Errorf("non-string: %s", rbacErrs(t, errs))
	}
	// Both fields are validated and both errors reported, in declaration order.
	_, _, errs = parseMemberCreate([]byte(`{"role":"owner"}`))
	if len(errs) != 2 || errs[0].Loc[1] != "username" || errs[1].Loc[1] != "role" {
		t.Errorf("two errors: %s", rbacErrs(t, errs))
	}
}
