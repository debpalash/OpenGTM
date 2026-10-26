//go:build integration

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

func (e settingsEnv) cleanIntegrations(t *testing.T) {
	t.Helper()
	reg, _ := settings.Load()
	var keys []string
	for _, it := range reg.Integrations {
		for _, f := range it.Fields {
			keys = append(keys, f.Key)
		}
	}
	clean := func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM app_settings WHERE key = ANY($1)`, keys)
	}
	clean()
	t.Cleanup(clean)
}

func (e settingsEnv) wsEnv() wsEnv { return wsEnv{env: e.env, pool: e.pool} }

func intFieldsByKey(t *testing.T, body []byte, id string) map[string]map[string]any {
	t.Helper()
	var b struct {
		Integrations []struct {
			ID     string           `json:"id"`
			Fields []map[string]any `json:"fields"`
		} `json:"integrations"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	for _, it := range b.Integrations {
		if it.ID == id {
			out := map[string]map[string]any{}
			for _, f := range it.Fields {
				out[f["key"].(string)] = f
			}
			return out
		}
	}
	t.Fatalf("integration %s missing in %s", id, body)
	return nil
}

func TestIntegrationViewNeverEchoesSecretsHTTP(t *testing.T) {
	e := newSettingsEnv(t)
	e.cleanIntegrations(t)
	_, adm := e.seedAdmin(t, "t7-adm1")
	e.seed(t, "t7-plain1", pythonHash, true, "user", false)
	rec, _ := e.put(t, adm, "/api/settings/integrations/salesforce",
		`{"values":{"SALESFORCE_INSTANCE_URL":"https://x.my.salesforce.com","SALESFORCE_ACCESS_TOKEN":"tok-secret-1234"}}`)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "tok-secret") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var put struct {
		Status string   `json:"status"`
		Saved  []string `json:"saved"`
		Int    struct {
			Connected bool `json:"connected"`
		} `json:"integration"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &put)
	if put.Status != "ok" || len(put.Saved) != 2 || put.Saved[0] != "SALESFORCE_INSTANCE_URL" || !put.Int.Connected {
		t.Fatalf("put body %s", rec.Body.String())
	}
	rec, _ = e.do("GET", "/api/settings/integrations", "", "", bearer("t7-plain1"))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "tok-secret") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	f := intFieldsByKey(t, rec.Body.Bytes(), "salesforce")
	if f["SALESFORCE_ACCESS_TOKEN"]["value"] != "" || f["SALESFORCE_ACCESS_TOKEN"]["masked"] != "…1234" ||
		f["SALESFORCE_INSTANCE_URL"]["value"] != "https://x.my.salesforce.com" {
		t.Fatalf("fields %v", f)
	}
}

func TestIntegrationPutIgnoresBlankAndForeignKeysHTTP(t *testing.T) {
	e := newSettingsEnv(t)
	e.cleanIntegrations(t)
	_, adm := e.seedAdmin(t, "t7-adm2")
	rec, _ := e.put(t, adm, "/api/settings/integrations/salesforce",
		`{"values":{"SALESFORCE_ACCESS_TOKEN":"  ","HUBSPOT_TOKEN":"foreign-1234","SALESFORCE_INSTANCE_URL":" https://t.test "}}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"saved":["SALESFORCE_INSTANCE_URL"]`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if v, _ := e.raw(t, "SALESFORCE_INSTANCE_URL"); v != "https://t.test" {
		t.Fatalf("url %q", v)
	}
	for _, k := range []string{"HUBSPOT_TOKEN", "SALESFORCE_ACCESS_TOKEN"} {
		if _, ok := e.raw(t, k); ok {
			t.Fatalf("%s written", k)
		}
	}
}

func TestIntegrationPutUnknown404(t *testing.T) {
	e := newSettingsEnv(t)
	_, adm := e.seedAdmin(t, "t7-adm3")
	rec, _ := e.put(t, adm, "/api/settings/integrations/nope", `{"values":{"A":"b"}}`)
	if rec.Code != 404 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Integration not found"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestIntegrationPutBodyValidation(t *testing.T) {
	e := newSettingsEnv(t)
	_, adm := e.seedAdmin(t, "t7-adm4")
	for name, body := range map[string]string{
		"missing": `{}`, "null": `{"values":null}`, "notdict": `{"values":[1]}`, "nonstring": `{"values":{"HUBSPOT_TOKEN":5}}`, "empty": ``,
	} {
		rec, _ := e.put(t, adm, "/api/settings/integrations/hubspot", body)
		if rec.Code != 422 {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// Body is validated before the integration id is looked up (FastAPI order).
	rec, _ := e.put(t, adm, "/api/settings/integrations/nope", `{}`)
	if rec.Code != 422 {
		t.Fatalf("validation-before-404: %d", rec.Code)
	}
}

func TestGlobalIntegrationSecretEncryptedAtRestHTTP(t *testing.T) {
	e := newSettingsEnv(t)
	e.cleanIntegrations(t)
	_, adm := e.seedAdmin(t, "t7-adm5")
	if rec, _ := e.put(t, adm, "/api/settings/integrations/hubspot", `{"values":{"HUBSPOT_TOKEN":"pat-na1-secret-value"}}`); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	raw, ok := e.raw(t, "HUBSPOT_TOKEN")
	if !ok || !strings.HasPrefix(raw, "enc:v1:") || strings.Contains(raw, "pat-na1") {
		t.Fatalf("not encrypted: %q", raw)
	}
	// Legacy plaintext (pre-encryption script) is still read and masked.
	if _, err := e.pool.Exec(context.Background(), `UPDATE app_settings SET value='legacy-plain-5678' WHERE key='HUBSPOT_TOKEN'`); err != nil {
		t.Fatal(err)
	}
	rec, _ := e.do("GET", "/api/settings/integrations", "", "", adm)
	if f := intFieldsByKey(t, rec.Body.Bytes(), "hubspot")["HUBSPOT_TOKEN"]; f["masked"] != "…5678" {
		t.Fatalf("legacy plaintext %v", f)
	}
}

func TestIntegrationPutRequiresGlobalAdminHTTP(t *testing.T) {
	e := newSettingsEnv(t)
	e.cleanIntegrations(t)
	e.seed(t, "t7-plain2", pythonHash, true, "user", false)
	rec, _ := e.put(t, bearer("t7-plain2"), "/api/settings/integrations/hubspot", `{"values":{"HUBSPOT_TOKEN":"x-token-1234"}}`)
	if rec.Code != 403 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Not authorized"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, ok := e.raw(t, "HUBSPOT_TOKEN"); ok {
		t.Fatal("non-admin wrote")
	}
	if rec, _ = e.do("GET", "/api/settings/integrations", "", "", nil); rec.Code != 401 {
		t.Fatalf("no auth: %d", rec.Code)
	}
}

// wsInteg seeds ws-<tag> with owner/editor/viewer users and returns their headers.
type wsInteg struct {
	ws                  string
	owner, editor, view map[string]string
	editorID, viewID    int32
}

func (e settingsEnv) wsInteg(t *testing.T, tag string) wsInteg {
	t.Helper()
	w := e.wsEnv()
	oid, oh := w.user(t, "t7-o-"+tag)
	eid, eh := w.user(t, "t7-e-"+tag)
	vid, vh := w.user(t, "t7-v-"+tag)
	ws := "t7-ws-" + tag
	w.mk(t, ws, ws, ws, oid, 1700000000, map[int32]string{oid: "owner", eid: "editor", vid: "viewer"})
	t.Cleanup(func() {
		c := context.Background()
		_ = tenancy.InWorkspace(c, e.pool, ws, func(tx pgx.Tx) error {
			_, _ = tx.Exec(c, `DELETE FROM workspace_member_permissions WHERE workspace_id=$1`, ws)
			return nil
		})
	})
	return wsInteg{ws, oh, eh, vh, eid, vid}
}

func (e settingsEnv) rawWS(t *testing.T, ws, key string) (string, bool) {
	t.Helper()
	var v string
	found := false
	err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
		err := tx.QueryRow(context.Background(), `SELECT value FROM workspace_settings WHERE workspace_id=$1 AND key=$2`, ws, key).Scan(&v)
		found = err == nil
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return v, found
}

func TestWorkspaceIntegrationSourcesWorkspaceGlobalUnsetHTTP(t *testing.T) {
	e := newSettingsEnv(t)
	e.cleanIntegrations(t)
	w := e.wsInteg(t, "src")
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO app_settings (key,value) VALUES ('SALESFORCE_INSTANCE_URL','https://global.test')`); err != nil {
		t.Fatal(err)
	}
	path := "/api/settings/workspace-integrations/salesforce?workspace_id=" + w.ws
	rec, _ := e.put(t, w.editor, path, `{"values":{"SALESFORCE_ACCESS_TOKEN":"ws-secret-7777"}}`)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "ws-secret") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec, _ = e.do("GET", "/api/settings/workspace-integrations?workspace_id="+w.ws, "", "", w.view)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "ws-secret") || strings.Contains(rec.Body.String(), "global.test") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var top struct {
		WS string `json:"workspace_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &top)
	f := intFieldsByKey(t, rec.Body.Bytes(), "salesforce")
	if top.WS != w.ws || f["SALESFORCE_INSTANCE_URL"]["source"] != "global" || f["SALESFORCE_ACCESS_TOKEN"]["source"] != "workspace" ||
		f["SALESFORCE_ACCESS_TOKEN"]["masked"] != "set" || f["SALESFORCE_ACCESS_TOKEN"]["value"] != "" {
		t.Fatalf("fields %v top %v", f, top)
	}
	if h := intFieldsByKey(t, rec.Body.Bytes(), "hubspot")["HUBSPOT_TOKEN"]; h["source"] != "unset" || h["set"] != false || h["masked"] != "" {
		t.Fatalf("hubspot %v", h)
	}
}

func TestWorkspaceIntegrationPutEncryptsAtRestHTTP(t *testing.T) {
	e := newSettingsEnv(t)
	e.cleanIntegrations(t)
	w := e.wsInteg(t, "enc")
	rec, _ := e.put(t, w.owner, "/api/settings/workspace-integrations/hubspot?workspace_id="+w.ws,
		`{"values":{"HUBSPOT_TOKEN":" pat-ws-secret ","OTHER":"x"}}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"saved":["HUBSPOT_TOKEN"]`) || !strings.Contains(rec.Body.String(), `"workspace_id":"`+w.ws+`"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	raw, ok := e.rawWS(t, w.ws, "HUBSPOT_TOKEN")
	if !ok || !strings.HasPrefix(raw, "enc:v1:") || strings.Contains(raw, "pat-ws-secret") {
		t.Fatalf("workspace row %q", raw)
	}
	if _, ok := e.rawWS(t, w.ws, "OTHER"); ok {
		t.Fatal("foreign key written")
	}
	if _, ok := e.raw(t, "HUBSPOT_TOKEN"); ok {
		t.Fatal("workspace write leaked into app_settings")
	}
}

func TestWorkspaceIntegrationViewerForbiddenHTTP(t *testing.T) {
	e := newSettingsEnv(t)
	e.cleanIntegrations(t)
	w := e.wsInteg(t, "view")
	path := "/api/settings/workspace-integrations/hubspot?workspace_id=" + w.ws
	rec, _ := e.put(t, w.view, path, `{"values":{"HUBSPOT_TOKEN":"x-token-1234"}}`)
	if rec.Code != 403 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Insufficient workspace role"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, ok := e.rawWS(t, w.ws, "HUBSPOT_TOKEN"); ok {
		t.Fatal("viewer wrote")
	}
	// Unknown integration is 404 before membership/permission (FastAPI order).
	if rec, _ = e.put(t, w.view, "/api/settings/workspace-integrations/nope?workspace_id="+w.ws, `{"values":{}}`); rec.Code != 404 {
		t.Fatalf("unknown: %d", rec.Code)
	}
	if rec, _ = e.do("GET", "/api/settings/workspace-integrations?workspace_id="+w.ws, "", "", w.view); rec.Code != 200 {
		t.Fatalf("viewer GET: %d", rec.Code)
	}
}

func TestWorkspaceIntegrationOtherWorkspace403HTTP(t *testing.T) {
	e := newSettingsEnv(t)
	a, b := e.wsInteg(t, "a"), e.wsInteg(t, "b")
	for _, c := range []struct{ m, p string }{
		{"GET", "/api/settings/workspace-integrations?workspace_id=" + b.ws},
		{"PUT", "/api/settings/workspace-integrations/hubspot?workspace_id=" + b.ws},
	} {
		rec, _ := e.do(c.m, c.p, "application/json", `{"values":{"HUBSPOT_TOKEN":"x-token-1234"}}`, a.owner)
		if rec.Code != 403 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Not a member of this workspace"}` {
			t.Fatalf("%s: %d %s", c.m, rec.Code, rec.Body.String())
		}
	}
	if _, ok := e.rawWS(t, b.ws, "HUBSPOT_TOKEN"); ok {
		t.Fatal("cross-workspace write")
	}
}

func TestWorkspaceIntegrationNoActiveWorkspace400HTTP(t *testing.T) {
	e := newSettingsEnv(t)
	e.seed(t, "t7-lonely", pythonHash, true, "user", false)
	h := bearer("t7-lonely")
	for _, c := range []struct{ m, p string }{
		{"GET", "/api/settings/workspace-integrations"},
		{"PUT", "/api/settings/workspace-integrations/hubspot"},
	} {
		rec, _ := e.do(c.m, c.p, "application/json", `{"values":{"HUBSPOT_TOKEN":"x-token-1234"}}`, h)
		if rec.Code != 400 || strings.TrimSpace(rec.Body.String()) != `{"detail":"No active workspace"}` {
			t.Fatalf("%s: %d %s", c.m, rec.Code, rec.Body.String())
		}
	}
}

func TestWorkspaceIntegrationUsesActiveWorkspaceWhenOmitted(t *testing.T) {
	e := newSettingsEnv(t)
	e.cleanIntegrations(t)
	w := e.wsInteg(t, "act")
	e.wsEnv().setActive(t, e.wsEnv().mustUser(t, "t7-e-act").ID, w.ws)
	rec, _ := e.put(t, w.editor, "/api/settings/workspace-integrations/hubspot", `{"values":{"HUBSPOT_TOKEN":"active-ws-token"}}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"workspace_id":"`+w.ws+`"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, ok := e.rawWS(t, w.ws, "HUBSPOT_TOKEN"); !ok {
		t.Fatal("not written to the active workspace")
	}
}

func TestWorkspaceIntegrationPermissionDenyOverrideHTTP(t *testing.T) {
	e := newSettingsEnv(t)
	e.cleanIntegrations(t)
	w := e.wsInteg(t, "perm")
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, e.pool, w.ws, func(tx pgx.Tx) error {
		for _, r := range [][3]any{{w.editorID, "integrations.manage", "deny"}, {w.viewID, "integrations.manage", "allow"}} {
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_member_permissions (workspace_id,user_id,permission,effect,updated_at) VALUES ($1,$2,$3,$4,0)`,
				w.ws, r[0], r[1], r[2]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/settings/workspace-integrations/hubspot?workspace_id=" + w.ws
	body := `{"values":{"HUBSPOT_TOKEN":"override-token-1"}}`
	if rec, _ := e.put(t, w.editor, path, body); rec.Code != 403 || !strings.Contains(rec.Body.String(), "Insufficient workspace role") {
		t.Fatalf("denied editor: %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := e.put(t, w.view, path, body); rec.Code != 200 {
		t.Fatalf("allowed viewer: %d %s", rec.Code, rec.Body.String())
	}
}

// TestIntegrationRoutesMatchFixtures replays the recorded, order-dependent
// FastAPI sequence (testdata/contract/integrations). alice is a global admin and
// owner of the workspace, bob a plain "member" of it.
func TestIntegrationRoutesMatchFixtures(t *testing.T) {
	e := newSettingsEnv(t)
	e.cleanIntegrations(t)
	w := e.wsEnv()
	aid, ah := w.user(t, "t7-fx-alice")
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET is_admin=true, role='admin' WHERE id=$1`, aid); err != nil {
		t.Fatal(err)
	}
	bid, bh := w.user(t, "t7-fx-bob")
	ws := "t7-fx-ws"
	w.mk(t, ws, "Alpha", ws, aid, 1788000000, map[int32]string{aid: "owner", bid: "member"})
	w.setActive(t, aid, ws)
	q := "?workspace_id=" + ws
	steps := []struct {
		name, method, path, body string
		hdr                      map[string]string
	}{
		{"int_list_before", "GET", "/api/settings/integrations", "", ah},
		{"int_list_no_auth", "GET", "/api/settings/integrations", "", nil},
		{"int_put_unknown", "PUT", "/api/settings/integrations/nope", `{"values":{"A":"b"}}`, ah},
		{"int_put_non_admin", "PUT", "/api/settings/integrations/salesforce", `{"values":{"SALESFORCE_INSTANCE_URL":"https://x.test"}}`, bh},
		{"int_put_missing_values", "PUT", "/api/settings/integrations/salesforce", `{}`, ah},
		{"int_put_non_string_value", "PUT", "/api/settings/integrations/salesforce", `{"values":{"SALESFORCE_INSTANCE_URL":5}}`, ah},
		{"int_put_ok", "PUT", "/api/settings/integrations/salesforce",
			`{"values":{"SALESFORCE_INSTANCE_URL":" https://contract.my.salesforce.com ","SALESFORCE_ACCESS_TOKEN":"sf-contract-token-9876","HUBSPOT_TOKEN":"foreign-key-ignored","NOT_A_FIELD":"ignored"}}`, ah},
		{"int_put_blank_noop", "PUT", "/api/settings/integrations/salesforce", `{"values":{"SALESFORCE_ACCESS_TOKEN":"   ","SALESFORCE_INSTANCE_URL":""}}`, ah},
		{"int_put_short_secret", "PUT", "/api/settings/integrations/airtable", `{"values":{"AIRTABLE_TOKEN":"abc"}}`, ah},
		{"int_list_after", "GET", "/api/settings/integrations", "", bh},
		{"ws_list_alpha_before", "GET", "/api/settings/workspace-integrations" + q, "", ah},
		{"ws_put_member_forbidden", "PUT", "/api/settings/workspace-integrations/hubspot" + q, `{"values":{"HUBSPOT_TOKEN":"ws-contract-token-4321"}}`, bh},
		{"ws_put_unknown", "PUT", "/api/settings/workspace-integrations/nope" + q, `{"values":{"A":"b"}}`, ah},
		{"ws_put_owner_ok", "PUT", "/api/settings/workspace-integrations/hubspot" + q, `{"values":{"HUBSPOT_TOKEN":" ws-contract-token-4321 ","NOT_A_FIELD":"x"}}`, ah},
		{"ws_list_alpha_after", "GET", "/api/settings/workspace-integrations" + q, "", bh},
		{"ws_list_active", "GET", "/api/settings/workspace-integrations", "", ah},
		{"ws_list_other_workspace", "GET", "/api/settings/workspace-integrations?workspace_id=ws-missing", "", bh},
		{"ws_put_other_workspace", "PUT", "/api/settings/workspace-integrations/hubspot?workspace_id=ws-missing", `{"values":{"HUBSPOT_TOKEN":"nope-token-0000"}}`, bh},
		{"ws_list_no_auth", "GET", "/api/settings/workspace-integrations", "", nil},
	}
	for _, s := range steps {
		fx := contract.Load(t, "integrations/"+s.name)
		_, got := e.do(s.method, s.path, "application/json", s.body, s.hdr)
		t.Run(s.name, func(t *testing.T) {
			contract.AssertEqual(t, got, fx.Response, contract.IgnorePaths("$.workspace_id"))
		})
	}
}
