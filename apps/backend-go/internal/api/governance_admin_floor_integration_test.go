//go:build integration

package api

import (
	"context"
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
