package mcp

import (
	"context"
	"errors"
	"sort"
	"testing"
)

func TestBearerTokenParsing(t *testing.T) {
	cases := []struct {
		in   string
		tok  string
		isOK bool
	}{
		{"bearer x", "x", true},
		{"Bearer x", "x", true},
		{"BEARER x", "x", true},
		{"Bearer", "", true},
		{"Bearer ", "", true},
		{"Bearer a b", "a b", true},
		{"Basic x", "", false},
		{"", "", false},
		{" x", "", false},
		{"Bearerx", "", false},
	}
	for _, c := range cases {
		tok, ok := BearerToken(c.in)
		if tok != c.tok || ok != c.isOK {
			t.Errorf("BearerToken(%q) = %q,%v want %q,%v", c.in, tok, ok, c.tok, c.isOK)
		}
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestCapabilityVocabularyVerbatim(t *testing.T) {
	eq := func(name string, got map[string]bool, want ...string) {
		t.Helper()
		g := keys(got)
		sort.Strings(want)
		if len(g) != len(want) {
			t.Fatalf("%s = %v want %v", name, g, want)
		}
		for i := range g {
			if g[i] != want[i] {
				t.Fatalf("%s = %v want %v", name, g, want)
			}
		}
	}
	eq("ReadCaps", ReadCaps, "leads:read")
	eq("WriteCaps", WriteCaps, "leads:write", "sequences:enroll", "workbooks:write", "automations:write", "outreach:send", "leads:delete", "workbooks:delete")
	eq("AdminOnlyCaps", AdminOnlyCaps, "automations:write", "outreach:send", "leads:delete", "workbooks:delete")
	eq("AllCaps", AllCaps, "leads:read", "leads:write", "sequences:enroll", "workbooks:write", "automations:write", "outreach:send", "leads:delete", "workbooks:delete")
	if len(WriteRoles) != 3 || WriteRoles[0] != "admin" || WriteRoles[1] != "member" || WriteRoles[2] != "editor" {
		t.Fatalf("WriteRoles = %v", WriteRoles)
	}
	if len(AdminRoles) != 1 || AdminRoles[0] != "admin" {
		t.Fatalf("AdminRoles = %v", AdminRoles)
	}
}

func TestRequireRoleSelfHostPasses(t *testing.T) {
	// UserID nil (keyless self-host) passes without touching the database.
	if err := RequireRole(context.Background(), Deps{}, Ctx{WorkspaceID: "w"}, AdminRoles...); err != nil {
		t.Fatalf("self-host: %v", err)
	}
}

func TestAuthErrorIsError(t *testing.T) {
	var err error = &AuthError{Msg: "invalid MCP token"}
	var ae *AuthError
	if !errors.As(err, &ae) || err.Error() != "invalid MCP token" {
		t.Fatal("AuthError must carry Msg as its text")
	}
}

func TestResolveWithoutRunnerIsUnavailable(t *testing.T) {
	_, err := Resolve(context.Background(), Deps{Cfg: Config{RequireAuth: true}}, "ycp_x")
	if !IsUnavailable(err) {
		t.Fatalf("want unavailable, got %v", err)
	}
}
