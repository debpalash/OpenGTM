//go:build integration

package api

import (
	"strings"
	"testing"
)

// Final review M1: a workspace-scoped (SSO) session is confined to its workspace on the
// workspace-integrations routes, and enforce_sso applies there as on every workspace route.
func TestWorkspaceIntegrationSSOScopedSessionConfined(t *testing.T) {
	e := newSettingsEnv(t)
	e.cleanIntegrations(t)
	w := e.wsEnv()
	uid, _ := w.user(t, "t7-sso-u")
	oid, _ := w.user(t, "t7-sso-o")
	a, b := "t7-sso-wsa", "t7-sso-wsb"
	w.mk(t, a, a, a, oid, 1700000000, map[int32]string{oid: "owner", uid: "editor"})
	w.mk(t, b, b, b, oid, 1700000001, map[int32]string{oid: "owner", uid: "editor"})
	w.setActive(t, uid, b)
	sso := ssoBearer(t, "t7-sso-u", a)
	body := `{"values":{"HUBSPOT_TOKEN":"scoped-token-1234"}}`

	for _, c := range []struct{ m, p string }{
		{"GET", "/api/settings/workspace-integrations?workspace_id=" + b},
		{"PUT", "/api/settings/workspace-integrations/hubspot?workspace_id=" + b},
	} {
		rec, _ := e.do(c.m, c.p, "application/json", body, sso)
		if rec.Code != 403 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Workspace access denied"}` {
			t.Fatalf("%s other workspace: %d %s", c.m, rec.Code, rec.Body.String())
		}
	}
	if _, ok := e.rawWS(t, b, "HUBSPOT_TOKEN"); ok {
		t.Fatal("scoped session wrote another workspace's integration")
	}
	// Without ?workspace_id= the scoped workspace replaces the stored active one (b).
	rec, _ := e.put(t, sso, "/api/settings/workspace-integrations/hubspot", body)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"workspace_id":"`+a+`"`) {
		t.Fatalf("scoped default: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := e.rawWS(t, b, "HUBSPOT_TOKEN"); ok {
		t.Fatal("scoped session wrote its stored active workspace")
	}
	// Its own workspace, named explicitly, is fine.
	if rec, _ := e.do("GET", "/api/settings/workspace-integrations?workspace_id="+a, "", "", sso); rec.Code != 200 {
		t.Fatalf("own workspace: %d %s", rec.Code, rec.Body.String())
	}
}

func TestWorkspaceIntegrationEnforceSSO(t *testing.T) {
	e := newSettingsEnv(t)
	e.cleanIntegrations(t)
	w := e.wsInteg(t, "enfsso")
	setSetting(t, e.pool, w.ws, "OIDC_CONFIG", `{"enforce_sso":true}`)
	body := `{"values":{"HUBSPOT_TOKEN":"enforced-token-1234"}}`
	for _, c := range []struct{ m, p string }{
		{"GET", "/api/settings/workspace-integrations?workspace_id=" + w.ws},
		{"PUT", "/api/settings/workspace-integrations/hubspot?workspace_id=" + w.ws},
	} {
		rec, _ := e.do(c.m, c.p, "application/json", body, w.editor) // password session
		if rec.Code != 403 || strings.TrimSpace(rec.Body.String()) != `{"detail":"`+ssoRequiredDetail+`"}` {
			t.Fatalf("%s password session: %d %s", c.m, rec.Code, rec.Body.String())
		}
	}
	if _, ok := e.rawWS(t, w.ws, "HUBSPOT_TOKEN"); ok {
		t.Fatal("password session wrote an enforce_sso workspace")
	}
	// An SSO session of the same editor, and the owner's password session, pass.
	if rec, _ := e.put(t, ssoBearer(t, "t7-e-enfsso", w.ws), "/api/settings/workspace-integrations/hubspot?workspace_id="+w.ws, body); rec.Code != 200 {
		t.Fatalf("sso session: %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := e.do("GET", "/api/settings/workspace-integrations?workspace_id="+w.ws, "", "", w.owner); rec.Code != 200 {
		t.Fatalf("owner: %d %s", rec.Code, rec.Body.String())
	}
}
