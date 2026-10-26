//go:build integration

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Deliberate Go hardening (Python allows governance.manage override holders): SSO config,
// SCIM token, legal hold / retention writes and enforce need role >= admin even with an allow override.

func grantManage(t *testing.T, exec func(string, ...any), ws string, uid int32) {
	t.Helper()
	exec(`INSERT INTO workspace_member_permissions (workspace_id, user_id, permission, effect, updated_at) VALUES ($1,$2,'governance.manage','allow',1)
		ON CONFLICT (workspace_id, user_id, permission) DO UPDATE SET effect='allow'`, ws, uid)
}

func TestSSOAndSCIMTokenNeedAdminRoleEvenWithOverride(t *testing.T) {
	e := newSSOEnv(t)
	run := func(sql string, a ...any) { e.exec(t, sql, a...) }
	t.Cleanup(func() {
		_, _ = e.owner.Exec(context.Background(), `DELETE FROM workspace_member_permissions WHERE workspace_id='ws-alpha'`)
	})
	routes := []struct{ method, path, body string }{
		{"GET", "/api/governance/sso", ""}, {"PUT", "/api/governance/sso", `{"enabled":false}`},
		{"GET", "/api/governance/scim-token", ""}, {"POST", "/api/governance/scim-token", ""}, {"DELETE", "/api/governance/scim-token", ""},
	}
	for _, role := range []string{"viewer", "member", "editor"} {
		run(`UPDATE workspace_members SET role=$1 WHERE workspace_id='ws-alpha' AND user_id=$2`, role, e.ids["erin"])
		grantManage(t, run, "ws-alpha", e.ids["erin"])
		for _, r := range routes {
			if code, _, raw := e.req("erin", "ws-alpha", r.method, r.path, r.body); code != 403 {
				t.Errorf("%s %s %s with override: %d %s, want 403", role, r.method, r.path, code, raw)
			}
		}
	}
	run(`UPDATE workspace_members SET role='admin' WHERE workspace_id='ws-alpha' AND user_id=$1`, e.ids["erin"])
	for _, r := range routes {
		if code, _, raw := e.req("erin", "ws-alpha", r.method, r.path, r.body); code == 403 {
			t.Errorf("admin %s %s: 403 %s", r.method, r.path, raw)
		}
	}
	if code, _, raw := e.req("alice", "ws-alpha", "GET", "/api/governance/sso", ""); code != 200 {
		t.Errorf("owner: %d %s", code, raw)
	}
}

func TestRetentionWritesAndEnforceNeedAdminRoleEvenWithOverride(t *testing.T) {
	e := newGREnv(t)
	run := func(sql string, a ...any) { e.exec(t, sql, a...) }
	t.Cleanup(func() {
		_, _ = e.owner.Exec(context.Background(), `DELETE FROM workspace_member_permissions WHERE workspace_id='ws-alpha'`)
	})
	routes := []struct{ method, path, body string }{
		{"PUT", "/api/governance/retention", `{"enabled":true,"legal_hold":false}`},
		{"POST", "/api/governance/retention/enforce", `{"confirmation":"PURGE EXPIRED DATA"}`},
	}
	for _, role := range []string{"viewer", "member", "editor"} {
		run(`UPDATE workspace_members SET role=$1 WHERE workspace_id='ws-alpha' AND user_id=$2`, role, e.ids["erin"])
		grantManage(t, run, "ws-alpha", e.ids["erin"])
		for _, r := range routes {
			if code, _, raw := e.req("erin", "ws-alpha", r.method, r.path, r.body); code != 403 {
				t.Errorf("%s %s %s with override: %d %s, want 403", role, r.method, r.path, code, raw)
			}
		}
	}
	run(`UPDATE workspace_members SET role='admin' WHERE workspace_id='ws-alpha' AND user_id=$1`, e.ids["erin"])
	for _, r := range routes {
		if code, _, raw := e.req("erin", "ws-alpha", r.method, r.path, r.body); code == 403 {
			t.Errorf("admin %s %s: 403 %s", r.method, r.path, raw)
		}
	}
}

func (e ssoEnv) scim(method, path, body, token string) (int, string) {
	ctype := ""
	if body != "" {
		ctype = "application/scim+json"
	}
	rec, _ := e.do(method, path, ctype, body, map[string]string{"Authorization": "Bearer " + token})
	return rec.Code, rec.Body.String()
}

// SCIM deprovision must never strip the workspace's owner (it would leave nobody holding owner
// permissions); ordinary members still deprovision (409 is a per-user SCIM error, IdP sync continues).
func TestSCIMDeprovisionRefusesWorkspaceOwner(t *testing.T) {
	e := newSSOEnv(t)
	t.Cleanup(func() { // registered last, runs first: the mappings reference the workspace
		_, _ = e.owner.Exec(context.Background(), `DELETE FROM workspace_scim_users WHERE workspace_id IN ('ws-alpha','ws-beta')`)
	})
	_, _, raw := e.req("alice", "ws-alpha", "POST", "/api/governance/scim-token", "")
	tok := scimToken(t, raw)
	base := "/scim/v2/alpha/Users"
	mk := func(name string) string {
		code, body := e.scim("POST", base, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"`+name+`","active":true}`, tok)
		var u struct {
			ID string `json:"id"`
		}
		if code/100 != 2 || json.Unmarshal([]byte(body), &u) != nil || u.ID == "" {
			t.Fatalf("provision %s: %d %s", name, code, body)
		}
		return u.ID
	}
	aliceID := mk("alice")
	if code, body := e.scim("DELETE", base+"/"+aliceID, "", tok); code != 409 || !strings.Contains(body, "owner") {
		t.Errorf("delete owner: %d %s, want 409", code, body)
	}
	patch := `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"replace","path":"active","value":false}]}`
	if code, body := e.scim("PATCH", base+"/"+aliceID, patch, tok); code != 409 {
		t.Errorf("deactivate owner: %d %s, want 409", code, body)
	}
	if n := e.str(t, `SELECT count(*)::text FROM workspace_members WHERE workspace_id='ws-alpha' AND role='owner'`); n != "1" {
		t.Errorf("owner rows: %s", n)
	}
	erinID := mk("erin")
	if code, body := e.scim("DELETE", base+"/"+erinID, "", tok); code != 204 {
		t.Errorf("delete editor: %d %s", code, body)
	}
	if n := e.str(t, `SELECT count(*)::text FROM workspace_members WHERE workspace_id='ws-alpha' AND user_id=$1`, e.ids["erin"]); n != "0" {
		t.Errorf("erin still a member: %s", n)
	}
}

func TestSCIMTokenAndSSOResponsesAreNoStore(t *testing.T) {
	e := newSSOEnv(t)
	for _, r := range []struct{ method, path string }{{"POST", "/api/governance/scim-token"}, {"GET", "/api/governance/sso"}, {"GET", "/api/governance/scim-token"}} {
		hdr := bearer("alice")
		hdr["X-Workspace-Id"] = "ws-alpha"
		rec, _ := e.do(r.method, r.path, "", "", hdr)
		if rec.Code/100 != 2 || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s %s: %d Cache-Control %q", r.method, r.path, rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
}
