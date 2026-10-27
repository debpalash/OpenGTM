//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// Plan 10 Task 18: destination CRUD (GET/POST /api/audience-destinations, PATCH/DELETE .../{id}) against the recorded
// Python fixtures (testdata/contract/destinations/dest_{list,create,patch,delete}_*) over the plan 10 seed.

type destEnv struct{ audEnv }

// destTestGetenv widens the outbound namespace (R2) to the bare names the recorded fixtures use; the default
// namespace is exercised by TestOutboundRefPolicy.
func destTestGetenv(k string) string {
	if k == "OUTBOUND_SECRET_REF_PREFIXES" {
		return "WAREHOUSE_AUTH,SLACK_,T18_,MISSING,DEST_,WEBHOOK_"
	}
	return ""
}

const destPrefix = "/api/audience-destinations"

func newDestEnv(t *testing.T) destEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	env, err := secrets.NewEnvelope(secrets.Config{MasterKey: "t18-master", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Envelope: env, Registry: testRegistry(t),
		Getenv:         destTestGetenv,
		Flags:          config.Flags{PGLeadStore: true, Audiences: config.AudienceSettings{GoAudienceRoutesEnabled: true, GoDestRoutesEnabled: true, RefreshPageSize: 500}},
		AudienceEvents: nopEmitter{}}
	RegisterAudiences(r, d)
	RegisterDestinations(r, d)
	e := audEnv{mcpEnv: mcpEnv{env: destEnv0(st, r), pool: pool}, r: r}
	e.seed(t, "alice", pythonHash, true, "admin", true)
	e.seed(t, "bob", pythonHash, true, "user", false)
	e.seed(t, "erin", pythonHash, true, "user", false)
	alice, bob, erin := e.mustUserByName(t, "alice"), e.mustUserByName(t, "bob"), e.mustUserByName(t, "erin")
	e.workspace(t, "ws-alpha", alice.ID)
	e.workspace(t, "ws-beta", bob.ID)
	e.member(t, "ws-alpha", alice.ID, "owner")
	e.member(t, "ws-alpha", bob.ID, "viewer")
	e.member(t, "ws-alpha", erin.ID, "editor")
	e.member(t, "ws-beta", bob.ID, "owner")
	destPurge(t)
	t.Cleanup(func() { destPurge(t) })
	audLoadSeed(t)
	res := secrets.NewResolver(env, secrets.NewPgStore(pool), func(string) string { return "" })
	destSeedSecrets(t, res)
	// The recorded webhook fixture names a ref (MISSING) the Python recording accepted without a lookup; a workspace
	// secret of that name keeps the replayed create valid under the S-M3 check.
	if err := res.Set(context.Background(), "ws-alpha", "MISSING", "t18-missing-value"); err != nil {
		t.Fatal(err)
	}
	return destEnv{e}
}

func destEnv0(st *identity.Store, r *httpx.Router) env { return env{h: r.Handler(), st: st} }

func destPurge(t *testing.T) {
	t.Helper()
	audPurge(t)
	ctx := context.Background()
	c := audOwner(t)
	for _, q := range []string{
		`DELETE FROM workspace_settings WHERE workspace_id IN ('ws-alpha','ws-beta')`,
		`DELETE FROM app_settings WHERE key LIKE 'T18_%'`,
		`DELETE FROM workspace_member_permissions WHERE workspace_id IN ('ws-alpha','ws-beta')`,
	} {
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatalf("purge %q: %v", q, err)
		}
	}
}

// destSeedSecrets stores the seed's workspace secrets through the envelope, as the Python recording did.
func destSeedSecrets(t *testing.T, res *secrets.Resolver) {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/audiences/seed.json")
	if err != nil {
		t.Fatal(err)
	}
	var seed struct {
		Secrets map[string]map[string]string `json:"secrets"`
	}
	if err := json.Unmarshal(raw, &seed); err != nil {
		t.Fatal(err)
	}
	for ws, kv := range seed.Secrets {
		for k, v := range kv {
			if err := res.Set(context.Background(), ws, k, v); err != nil {
				t.Fatalf("seed secret %s/%s: %v", ws, k, err)
			}
		}
	}
}

// replay runs one recorded request, mapping recorded created ids to the Go ones.
func (e destEnv) replay(t *testing.T, name string, ids map[string]string, opts ...contract.Option) contract.Resp {
	t.Helper()
	fx := contract.Load(t, "destinations/"+name)
	user, ws := audFixtureActor(name)
	path := audIDRe.ReplaceAllStringFunc(fx.Request.Path, func(s string) string {
		if v, ok := ids[s]; ok {
			return v
		}
		return s
	})
	body := ""
	if len(fx.Request.Body) > 0 && string(fx.Request.Body) != "null" {
		body = string(fx.Request.Body)
	}
	_, got := e.call(user, ws, fx.Request.Method, path, body)
	if fx.Response.Status/100 == 2 && len(fx.Response.Body) > 0 && fx.Response.Body[0] == '{' {
		opts = append(opts, contract.IgnorePaths("$.id"), contract.IgnoreShapeOf("$.created_at", "$.updated_at"))
	}
	t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response, opts...) })
	return got
}

func destFixtureNames(t *testing.T, prefix string) []string {
	t.Helper()
	ents, err := os.ReadDir("../../testdata/contract/destinations")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ent := range ents {
		if n := strings.TrimSuffix(ent.Name(), ".json"); strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func (e destEnv) post(who, ws, body string) (int, map[string]any, string) {
	rec, _ := e.call(who, ws, "POST", destPrefix, body)
	return rec.Code, destObj(rec), rec.Body.String()
}

func (e destEnv) patchDest(who, ws, id, body string) (int, map[string]any, string) {
	rec, _ := e.call(who, ws, "PATCH", destPrefix+"/"+id, body)
	return rec.Code, destObj(rec), rec.Body.String()
}

func destObj(rec *httptest.ResponseRecorder) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return m
}

func destDetail(m map[string]any) string { s, _ := m["detail"].(string); return s }

func destOwnerInt(t *testing.T, sql string, args ...any) int {
	t.Helper()
	return audOwnerInt(t, sql, args...)
}

// destKeys lists an object's keys in document order.
func destKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s", raw)
	}
	var keys []string
	for dec.More() {
		k, _ := dec.Token()
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func TestDestinationCRUDMatchesFixtures(t *testing.T) {
	e := newDestEnv(t)
	ids := map[string]string{}

	// Phase 0: reads over the seed alone (exact). dest_list_beta_bob needs the pending run a beta refresh enqueued.
	for _, n := range []string{"dest_list_default", "dest_list_audience_act2", "dest_list_audience_act1", "dest_list_audience_unknown",
		"dest_list_audience_foreign", "dest_list_audience_empty_param", "dest_list_viewer_bob_alpha", "dest_list_no_auth", "dest_list_nonmember"} {
		e.replay(t, n, ids)
	}

	// Phase 1: creates that succeeded, in recorded id order (so updated_at order matches the recording).
	type created struct{ name, recID string }
	var creates []created
	var rest []string
	for _, n := range destFixtureNames(t, "dest_create_") {
		fx := contract.Load(t, "destinations/"+n)
		if fx.Response.Status != 201 {
			rest = append(rest, n)
			continue
		}
		var b struct{ ID string }
		_ = json.Unmarshal(fx.Response.Body, &b)
		creates = append(creates, created{n, b.ID})
	}
	sort.Slice(creates, func(i, j int) bool { return creates[i].recID < creates[j].recID })
	byName := map[string]string{}
	for _, c := range creates {
		got := e.replay(t, c.name, ids)
		var b struct{ ID string }
		_ = json.Unmarshal(got.Body, &b)
		ids[c.recID] = b.ID
		byName[c.name] = b.ID
	}
	for _, n := range rest { // refusals: 4xx
		e.replay(t, n, ids)
	}

	// Phase 2: patches in the recorded order (successes first, then the refusals).
	okPatches := []string{"dest_patch_config", "dest_patch_name", "dest_patch_enabled_false", "dest_patch_field_map", "dest_patch_empty_body",
		"dest_patch_config_null_salesforce", "dest_patch_warehouse_ok", "dest_patch_slack_ok", "dest_patch_ads_config", "dest_patch_field_map_null"}
	done := map[string]bool{}
	for _, n := range okPatches {
		e.replay(t, n, ids)
		done[n] = true
	}
	// Python answers 500 to these three (D3 known crashes): Go answers 409 (rename) and 422 (null on a NOT NULL column).
	d3 := map[string]int{"dest_patch_duplicate_name_500": 409, "dest_patch_name_null_500": 422, "dest_patch_enabled_null_500": 422}
	for n := range d3 {
		done[n] = true
	}
	for _, n := range destFixtureNames(t, "dest_patch_") {
		if !done[n] {
			e.replay(t, n, ids)
		}
	}
	for n, want := range d3 {
		fx := contract.Load(t, "destinations/"+n)
		path := audIDRe.ReplaceAllStringFunc(fx.Request.Path, func(s string) string { return ids[s] })
		rec, _ := e.call("alice", "ws-alpha", "PATCH", path, string(fx.Request.Body))
		if rec.Code != want {
			t.Errorf("%s: %d %s, want %d", n, rec.Code, rec.Body.String(), want)
		}
	}

	// Phase 3: the list after the patches (seeded rows exact, created rows by position).
	fx := contract.Load(t, "destinations/dest_list_after_patch")
	var want []map[string]any
	_ = json.Unmarshal(fx.Response.Body, &want)
	var opts []contract.Option
	for i, a := range want {
		if id, _ := a["id"].(string); !strings.HasPrefix(id, "dst-") {
			opts = append(opts, contract.IgnorePaths(fmt.Sprintf("$[%d].id", i)), contract.IgnoreShapeOf(fmt.Sprintf("$[%d].created_at", i), fmt.Sprintf("$[%d].updated_at", i)))
		}
	}
	e.replay(t, "dest_list_after_patch", ids, opts...)

	// Phase 4: deletes (the seeded destination with runs and live tokens goes through the cascade assertions below).
	for _, n := range []string{"dest_delete_created", "dest_delete_again_404", "dest_delete_by_erin", "dest_delete_unknown_404", "dest_delete_foreign_404",
		"dest_delete_viewer_403", "dest_delete_no_auth"} {
		e.replay(t, n, ids)
	}
	runs := destOwnerInt(t, `SELECT count(*) FROM destination_runs WHERE destination_id = 'dst-act-salesforce'`)
	toks := destOwnerInt(t, `SELECT count(*) FROM destination_inbound_tokens WHERE destination_id = 'dst-act-salesforce' AND revoked_at IS NULL`)
	if runs == 0 || toks == 0 {
		t.Fatalf("seed: dst-act-salesforce has %d runs, %d live tokens", runs, toks)
	}
	e.replay(t, "dest_delete_with_runs_and_tokens", ids)
	if n := destOwnerInt(t, `SELECT count(*) FROM destination_runs WHERE destination_id = 'dst-act-salesforce'`); n != 0 {
		t.Errorf("runs left after delete: %d", n)
	}
	if n := destOwnerInt(t, `SELECT count(*) FROM destination_inbound_tokens WHERE destination_id = 'dst-act-salesforce' AND revoked_at IS NULL`); n != 0 {
		t.Errorf("live tokens left after delete: %d", n)
	}
	_ = byName
}

func TestCreateCheckOrder(t *testing.T) {
	e := newDestEnv(t)
	bad := `{"audience_id":"%s","name":"%s","destination_type":"%s","config":%s}`
	// 1. body validation (422 list) precedes the audience lookup.
	code, m, _ := e.post("alice", "ws-alpha", `{"audience_id":"nope"}`)
	if _, isList := m["detail"].([]any); code != 422 || !isList {
		t.Fatalf("body validation first: %d %v", code, m)
	}
	// 2. unknown audience (404) precedes config validation (422).
	code, m, _ = e.post("alice", "ws-alpha", fmt.Sprintf(bad, "nope", "x", "webhook", `{}`))
	if code != 404 || destDetail(m) != "Audience not found" {
		t.Fatalf("audience before config: %d %v", code, m)
	}
	// 3. config validation precedes the secret lookup.
	code, m, _ = e.post("alice", "ws-alpha", fmt.Sprintf(bad, "aud-act-1", "x", "slack", `{"webhook_secret_ref":"NOPE","message_template":"`+strings.Repeat("a", 2001)+`"}`))
	if code != 422 || destDetail(m) != "slack message_template may not exceed 2000 characters" {
		t.Fatalf("config before secret: %d %v", code, m)
	}
	// 4. the secret lookup precedes the duplicate check (aud-act-2 already has "slack destination").
	code, m, _ = e.post("alice", "ws-alpha", fmt.Sprintf(bad, "aud-act-2", "slack destination", "slack", `{"webhook_secret_ref":"NOPE"}`))
	if code != 422 || destDetail(m) != "slack webhook_secret_ref was not found" {
		t.Fatalf("secret before duplicate: %d %v", code, m)
	}
	// 5. then the duplicate.
	code, m, _ = e.post("alice", "ws-alpha", fmt.Sprintf(bad, "aud-act-2", "slack destination", "slack", `{"webhook_secret_ref":"SLACK_WEBHOOK"}`))
	if code != 409 || destDetail(m) != "A destination with this name already exists for the audience" {
		t.Fatalf("duplicate: %d %v", code, m)
	}
}

// TestCreateEveryTypeValidation replays every recorded config refusal and acceptance of every destination type.
func TestCreateEveryTypeValidation(t *testing.T) {
	e := newDestEnv(t)
	n := 0
	for _, name := range destFixtureNames(t, "dest_create_cfg_") {
		e.replay(t, name, map[string]string{})
		n++
	}
	if n < 60 {
		t.Fatalf("only %d config fixtures", n)
	}
	// Every supported type is accepted with its minimal valid config.
	valid := map[string]string{
		"webhook": `{"url":"https://h.example.test/x"}`, "hubspot": `{}`, "salesforce": `{}`,
		"warehouse_http": `{"url":"https://w.example.test","header_secret_ref":"WAREHOUSE_AUTH"}`,
		"meta_ads":       `{"custom_audience_id":"1","consent_attested":true,"consent_source":"form"}`,
		"google_ads":     `{"customer_id":"1","user_list_id":"2","consent_attested":true,"consent_source":"form"}`,
		"linkedin_ads":   `{"segment_id":"1","consent_attested":true,"consent_source":"form"}`,
		"instantly":      `{"campaign_id":"c"}`, "smartlead": `{"campaign_id":"c"}`,
		"google_sheets": `{"spreadsheet_id":"s","columns":["a"]}`, "airtable": `{"base_id":"b","table":"t"}`,
		"slack": `{"webhook_secret_ref":"SLACK_WEBHOOK"}`,
	}
	for typ, cfg := range valid {
		code, m, raw := e.post("alice", "ws-alpha", fmt.Sprintf(`{"audience_id":"aud-act-1","name":"valid %s","destination_type":"%s","config":%s}`, typ, typ, cfg))
		if code != 201 {
			t.Errorf("%s: %d %s", typ, code, raw)
		}
		if m["destination_type"] != typ {
			t.Errorf("%s: type %v", typ, m["destination_type"])
		}
	}
}

func TestSecretRefMissing422(t *testing.T) {
	e := newDestEnv(t)
	post := func(typ, cfg string) (int, string) {
		code, m, _ := e.post("alice", "ws-alpha", fmt.Sprintf(`{"audience_id":"aud-act-1","name":"s %s %d","destination_type":"%s","config":%s}`, typ, time.Now().UnixNano(), typ, cfg))
		return code, destDetail(m)
	}
	cases := []struct{ typ, cfg, want string }{
		{"slack", `{"webhook_secret_ref":"NO_SUCH"}`, "slack webhook_secret_ref was not found"},
		{"slack", `{"webhook_secret_ref":"BETA_ONLY_SECRET"}`, "slack webhook_secret_ref was not found"}, // another workspace's secret
		{"warehouse_http", `{"url":"https://w.example.test","header_secret_ref":"NO_SUCH"}`, "warehouse_http header_secret_ref was not found"},
		{"warehouse_http", `{"url":"https://w.example.test","header_secret_ref":"BETA_ONLY_SECRET"}`, "warehouse_http header_secret_ref was not found"},
	}
	for _, c := range cases {
		if code, d := post(c.typ, c.cfg); code != 422 || d != c.want {
			t.Errorf("%s %s: %d %q", c.typ, c.cfg, code, d)
		}
	}
	// A global (app-level) value or an env value does not satisfy the lookup: references resolve to workspace secrets only.
	if _, err := audOwner(t).Exec(context.Background(), `INSERT INTO app_settings (key, value) VALUES ('T18_GLOBAL_HOOK', 'https://hooks.example.test/g') ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}
	if code, d := post("slack", `{"webhook_secret_ref":"T18_GLOBAL_HOOK"}`); code != 422 || d != "slack webhook_secret_ref was not found" {
		t.Errorf("global secret: %d %q", code, d)
	}
	if code, d := post("warehouse_http", `{"url":"https://w.example.test","header_secret_ref":"T18_GLOBAL_HOOK"}`); code != 422 || d != "warehouse_http header_secret_ref was not found" {
		t.Errorf("global warehouse secret: %d %q", code, d)
	}
	if code, d, _ := e.patchDest("alice", "ws-alpha", "dst-act-slack", `{"config":{"webhook_secret_ref":"T18_GLOBAL_HOOK"}}`); code != 422 || destDetail(d) != "slack webhook_secret_ref was not found" {
		t.Errorf("global secret patch: %d %v", code, d)
	}
	// An empty workspace value counts as unset.
	if _, err := audOwner(t).Exec(context.Background(), `INSERT INTO workspace_settings (workspace_id, key, value) VALUES ('ws-alpha', 'T18_EMPTY', '') ON CONFLICT (workspace_id, key) DO UPDATE SET value = ''`); err != nil {
		t.Fatal(err)
	}
	if code, d := post("slack", `{"webhook_secret_ref":"T18_EMPTY"}`); code != 422 {
		t.Errorf("empty secret: %d %q", code, d)
	}
	// PATCH re-checks the reference.
	code, m, _ := e.patchDest("alice", "ws-alpha", "dst-act-slack", `{"config":{"webhook_secret_ref":"NO_SUCH"}}`)
	if code != 422 || destDetail(m) != "slack webhook_secret_ref was not found" {
		t.Errorf("patch: %d %v", code, m)
	}
	// The secret value is never echoed or stored in the destination.
	code, _, raw := e.post("alice", "ws-alpha", `{"audience_id":"aud-act-1","name":"no echo","destination_type":"slack","config":{"webhook_secret_ref":"SLACK_WEBHOOK"}}`)
	if code != 201 || strings.Contains(raw, "hooks.slack.test") {
		t.Errorf("echo: %d %s", code, raw)
	}
	if n := destOwnerInt(t, `SELECT count(*) FROM audience_destinations WHERE config::text LIKE '%hooks.slack.test%'`); n != 0 {
		t.Errorf("secret value stored in %d destinations", n)
	}
}

func TestCreateDuplicate409(t *testing.T) {
	e := newDestEnv(t)
	body := `{"audience_id":"aud-act-1","name":"Twice","destination_type":"salesforce"}`
	if code, _, raw := e.post("alice", "ws-alpha", body); code != 201 {
		t.Fatalf("first: %d %s", code, raw)
	}
	code, m, _ := e.post("alice", "ws-alpha", body)
	if code != 409 || destDetail(m) != "A destination with this name already exists for the audience" {
		t.Fatalf("second: %d %v", code, m)
	}
	// The padded name collides with the stripped one; the same name in another audience is fine.
	if code, _, _ := e.post("alice", "ws-alpha", `{"audience_id":"aud-act-1","name":"  Twice ","destination_type":"salesforce"}`); code != 409 {
		t.Errorf("padded duplicate: %d", code)
	}
	if code, _, raw := e.post("alice", "ws-alpha", `{"audience_id":"aud-act-2","name":"Twice","destination_type":"salesforce"}`); code != 201 {
		t.Errorf("other audience: %d %s", code, raw)
	}
	// The refused insert leaves nothing behind.
	if n := destOwnerInt(t, `SELECT count(*) FROM audience_destinations WHERE name = 'Twice'`); n != 2 {
		t.Errorf("rows: %d", n)
	}
}

func TestPatchRenameDuplicate409(t *testing.T) {
	e := newDestEnv(t)
	// dst-act-slack and dst-act-webhook share aud-act-2: renaming onto the sibling is a 409 (Python: 500, D3).
	code, m, _ := e.patchDest("alice", "ws-alpha", "dst-act-slack", `{"name":"webhook destination","enabled":false,"field_map":{"a":"b"}}`)
	if code != 409 || destDetail(m) != "A destination with this name already exists for the audience" {
		t.Fatalf("rename duplicate: %d %v", code, m)
	}
	// Nothing of the refused patch was applied.
	code, _, raw := e.patchDest("alice", "ws-alpha", "dst-act-slack", `{}`)
	var got map[string]any
	_ = json.Unmarshal([]byte(raw), &got)
	if code != 200 || got["name"] != "slack destination" || got["enabled"] != true || len(got["field_map"].(map[string]any)) != 0 {
		t.Errorf("after refusal: %d %s", code, raw)
	}
	// Renaming to its own name is not a duplicate; renaming to another audience's name is fine.
	if code, _, raw := e.patchDest("alice", "ws-alpha", "dst-act-slack", `{"name":"slack destination"}`); code != 200 {
		t.Errorf("own name: %d %s", code, raw)
	}
	if code, _, raw := e.patchDest("alice", "ws-alpha", "dst-act-slack", `{"name":"Disabled webhook"}`); code != 200 {
		t.Errorf("name of another audience: %d %s", code, raw)
	}
}

func TestPatchConfigRevalidated(t *testing.T) {
	e := newDestEnv(t)
	cases := []struct {
		id, body string
		code     int
		detail   string
	}{
		// The stored type decides the rules: a webhook config against a hubspot destination is just a free-form dict...
		{"dst-act-hubspot", `{"config":{"url":"ftp://x"}}`, 200, ""},
		// ...but the webhook rules apply to a webhook,
		{"dst-act-webhook", `{"config":{"url":"ftp://x"}}`, 422, "webhook requires an http(s) URL without embedded credentials"},
		{"dst-act-webhook", `{"config":{"url":"https://h.example.test","token":"x"}}`, 422, "credentials must use workspace secrets, not destination config"},
		{"dst-act-webhook", `{"config":null}`, 422, "webhook requires an http(s) URL without embedded credentials"},
		{"dst-act-salesforce", `{"config":null}`, 200, ""},
		{"dst-act-meta_ads", `{"config":{"custom_audience_id":"1"}}`, 422, "ad destinations require consent_attested=true and a consent_source"},
		{"dst-act-warehouse_http", `{"config":{"url":"https://w.example.test","header_secret_ref":"NO_SUCH"}}`, 422, "warehouse_http header_secret_ref was not found"},
		{"dst-act-warehouse_http", `{"config":{"url":"https://w.example.test","header_secret_ref":"WAREHOUSE_AUTH","mode":"upsert"}}`, 200, ""},
	}
	for _, c := range cases {
		code, m, raw := e.patchDest("alice", "ws-alpha", c.id, c.body)
		if code != c.code || (c.detail != "" && destDetail(m) != c.detail) {
			t.Errorf("%s %s: %d %s", c.id, c.body, code, raw)
		}
	}
	// A refused re-validation changes nothing.
	code, _, raw := e.patchDest("alice", "ws-alpha", "dst-act-webhook", `{}`)
	if code != 200 || !strings.Contains(raw, "https://hooks.example.test/audience") {
		t.Errorf("config changed by a refused patch: %s", raw)
	}
	// Nested key order and values survive the round trip.
	code, _, raw = e.patchDest("alice", "ws-alpha", "dst-act-smartlead", `{"config":{"settings":{"z":1,"a":{"y":2,"b":[1,2]}},"campaign_id":"9"}}`)
	if code != 200 || !strings.Contains(raw, `"config":{"settings":{"z":1,"a":{"y":2,"b":[1,2]}},"campaign_id":"9"}`) {
		t.Errorf("order: %d %s", code, raw)
	}
}

func TestDestPatchOnlyGivenFields(t *testing.T) {
	e := newDestEnv(t)
	before := destOwnerInt(t, `SELECT count(*) FROM audience_destinations WHERE id = 'dst-act-hubspot' AND updated_at = '2026-08-28T12:01:00'`)
	if before != 1 {
		t.Fatal("seed")
	}
	// An empty body is a plain read: updated_at stays.
	if code, _, raw := e.patchDest("alice", "ws-alpha", "dst-act-hubspot", `{}`); code != 200 || !strings.Contains(raw, `"updated_at":"2026-08-28T12:01:00"`) {
		t.Errorf("empty: %d %s", code, raw)
	}
	// Only the sent fields change.
	code, m, raw := e.patchDest("alice", "ws-alpha", "dst-act-hubspot", `{"enabled":false}`)
	if code != 200 || m["enabled"] != false || m["name"] != "hubspot destination" || m["updated_at"] == "2026-08-28T12:01:00" {
		t.Errorf("enabled: %d %s", code, raw)
	}
	fm, _ := m["field_map"].(map[string]any)
	if fm["company"] != "name" || fm["contact_title"] != "jobtitle" {
		t.Errorf("field_map touched: %v", fm)
	}
	// field_map null clears it (Python stores JSON null and answers {}).
	if code, m, raw := e.patchDest("alice", "ws-alpha", "dst-act-hubspot", `{"field_map":null}`); code != 200 || len(m["field_map"].(map[string]any)) != 0 {
		t.Errorf("field_map null: %d %s", code, raw)
	}
	// Null on a NOT NULL column: Python 500s; Go answers 422 and applies nothing.
	for _, body := range []string{`{"name":null,"enabled":false}`, `{"enabled":null,"name":"changed"}`} {
		code, m, _ := e.patchDest("alice", "ws-alpha", "dst-act-hubspot", body)
		if _, isList := m["detail"].([]any); code != 422 || !isList {
			t.Errorf("%s: %d %v", body, code, m)
		}
	}
	if code, m, _ := e.patchDest("alice", "ws-alpha", "dst-act-hubspot", `{}`); code != 200 || m["name"] != "hubspot destination" {
		t.Errorf("null patch applied something: %v", m)
	}
	// Pydantic refusals: 422 before the 404.
	for _, body := range []string{`{"name":"  "}`, `{"name":"` + strings.Repeat("n", 201) + `"}`, `{"field_map":{"a":1}}`, `{"enabled":"perhaps"}`, `{"config":[]}`} {
		if code, _, raw := e.patchDest("alice", "ws-alpha", "does-not-exist", body); code != 422 {
			t.Errorf("%s: %d %s", body, code, raw)
		}
	}
}

func TestListLatestRunShape(t *testing.T) {
	e := newDestEnv(t)
	rec, _ := e.call("alice", "ws-alpha", "GET", destPrefix+"?audience_id=aud-act-2", "")
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var items []json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil || len(items) != 12 {
		t.Fatalf("%v %d items", err, len(items))
	}
	wantKeys := []string{"id", "audience_id", "name", "destination_type", "enabled", "config", "field_map", "health_status", "last_error",
		"last_success_at", "created_at", "updated_at", "latest_run"}
	runKeys := []string{"id", "destination_id", "status", "attempted", "succeeded", "failed", "skipped", "error", "started_at", "finished_at", "created_at"}
	withRun, without := 0, 0
	for _, it := range items {
		if k := destKeys(t, it); strings.Join(k, ",") != strings.Join(wantKeys, ",") {
			t.Fatalf("destination key order %v", k)
		}
		var o map[string]json.RawMessage
		_ = json.Unmarshal(it, &o)
		if string(o["latest_run"]) == "null" {
			without++
			continue
		}
		withRun++
		if k := destKeys(t, o["latest_run"]); strings.Join(k, ",") != strings.Join(runKeys, ",") {
			t.Fatalf("latest_run key order %v", k)
		}
	}
	if withRun == 0 || without == 0 {
		t.Errorf("with run %d, without %d", withRun, without)
	}
	// The latest run is the newest by created_at, ties broken by the larger id; a naive timestamp renders without a zone.
	c := audOwner(t)
	var wantID string
	if err := c.QueryRow(context.Background(), `SELECT id FROM destination_runs WHERE destination_id = 'dst-act-webhook' ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&wantID); err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		var o struct {
			ID  string `json:"id"`
			Run *struct {
				ID string `json:"id"`
			} `json:"latest_run"`
		}
		_ = json.Unmarshal(it, &o)
		if o.ID == "dst-act-webhook" && (o.Run == nil || o.Run.ID != wantID) {
			t.Errorf("latest run %+v, want %s", o.Run, wantID)
		}
	}
	// Order: updated_at DESC, id DESC (a tie falls to the larger id).
	if _, err := c.Exec(context.Background(), `UPDATE audience_destinations SET updated_at = '2030-01-01T00:00:00' WHERE id IN ('dst-act-slack', 'dst-act-airtable')`); err != nil {
		t.Fatal(err)
	}
	rec, _ = e.call("alice", "ws-alpha", "GET", destPrefix+"?audience_id=aud-act-2", "")
	var top []struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &top)
	if top[0].ID != "dst-act-slack" || top[1].ID != "dst-act-airtable" {
		t.Errorf("tie order: %v", top[:2])
	}
	// The other workspace never shows up.
	rec, _ = e.call("alice", "ws-alpha", "GET", destPrefix, "")
	if strings.Contains(rec.Body.String(), "dst-beta-crm") {
		t.Error("foreign destination listed")
	}
}

func TestDeleteCascades(t *testing.T) {
	e := newDestEnv(t)
	for _, q := range []string{
		`SELECT count(*) FROM destination_runs WHERE destination_id = 'dst-act-webhook'`,
		`SELECT count(*) FROM destination_deliveries WHERE destination_id = 'dst-act-webhook'`,
	} {
		if destOwnerInt(t, q) == 0 {
			t.Fatalf("seed has none: %s", q)
		}
	}
	if _, err := audOwner(t).Exec(context.Background(), `INSERT INTO destination_inbound_tokens (id, workspace_id, destination_id, token_hash, prefix, created_at) VALUES ('t18-tok', 'ws-alpha', 'dst-act-webhook', repeat('a', 64), 'dsi_t18', now())`); err != nil {
		t.Fatal(err)
	}
	rec, _ := e.call("alice", "ws-alpha", "DELETE", destPrefix+"/dst-act-webhook", "")
	if rec.Code != 204 || rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("delete: %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
	}
	for _, q := range []string{
		`SELECT count(*) FROM audience_destinations WHERE id = 'dst-act-webhook'`,
		`SELECT count(*) FROM destination_runs WHERE destination_id = 'dst-act-webhook'`,
		`SELECT count(*) FROM destination_deliveries WHERE destination_id = 'dst-act-webhook'`,
		`SELECT count(*) FROM destination_inbound_tokens WHERE id = 't18-tok' AND revoked_at IS NULL`,
	} {
		if n := destOwnerInt(t, q); n != 0 {
			t.Errorf("%s -> %d", q, n)
		}
	}
	// The token row stays, revoked (Python parity: it is auth-plane evidence).
	if n := destOwnerInt(t, `SELECT count(*) FROM destination_inbound_tokens WHERE id = 't18-tok' AND revoked_at IS NOT NULL`); n != 1 {
		t.Errorf("revoked token rows: %d", n)
	}
	// Other destinations and the foreign workspace are untouched.
	if n := destOwnerInt(t, `SELECT count(*) FROM audience_destinations`); n != 16 {
		t.Errorf("destinations left: %d", n)
	}
	// Deleting the audience cascades its destinations (list after the audience delete).
	if rec, _ := e.call("alice", "ws-alpha", "DELETE", "/api/audiences/aud-act-2", ""); rec.Code != 204 {
		t.Fatalf("audience delete: %d %s", rec.Code, rec.Body.String())
	}
	rec, _ = e.call("alice", "ws-alpha", "GET", destPrefix, "")
	if strings.Contains(rec.Body.String(), `"audience_id":"aud-act-2"`) {
		t.Errorf("destinations of a deleted audience listed: %s", rec.Body.String())
	}
}

func TestDestEditorRequiredViewerReadsOnly(t *testing.T) {
	e := newDestEnv(t)
	createBody := `{"audience_id":"aud-act-1","name":"by role","destination_type":"salesforce"}`
	// Viewer (bob in ws-alpha): reads, no writes; the role check precedes body validation and the lookup.
	if rec, _ := e.call("bob", "ws-alpha", "GET", destPrefix, ""); rec.Code != 200 {
		t.Errorf("viewer list: %d", rec.Code)
	}
	for _, c := range [][3]string{{"POST", destPrefix, `{}`}, {"PATCH", destPrefix + "/nope", `{"name":null}`}, {"DELETE", destPrefix + "/nope", ""}} {
		rec, _ := e.call("bob", "ws-alpha", c[0], c[1], c[2])
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "Workspace permission denied: activation.write") {
			t.Errorf("viewer %s: %d %s", c[0], rec.Code, rec.Body.String())
		}
	}
	// Editor and admin write.
	if code, _, raw := e.post("erin", "ws-alpha", createBody); code != 201 {
		t.Errorf("editor create: %d %s", code, raw)
	}
	// A per-member deny beats the role; a per-member allow lifts a viewer.
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := audOwner(t).Exec(context.Background(), q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO workspace_member_permissions (workspace_id, user_id, permission, effect, updated_at) SELECT 'ws-alpha', id, 'activation.write', 'deny', 1 FROM users WHERE username = 'erin'`)
	if code, _, _ := e.post("erin", "ws-alpha", `{"audience_id":"aud-act-1","name":"denied","destination_type":"salesforce"}`); code != 403 {
		t.Errorf("denied editor: %d", code)
	}
	exec(`INSERT INTO workspace_member_permissions (workspace_id, user_id, permission, effect, updated_at) SELECT 'ws-alpha', id, 'activation.write', 'allow', 1 FROM users WHERE username = 'bob'`)
	if code, _, raw := e.post("bob", "ws-alpha", `{"audience_id":"aud-act-1","name":"allowed viewer","destination_type":"salesforce"}`); code != 201 {
		t.Errorf("allowed viewer: %d %s", code, raw)
	}
	// Unauthenticated and non-member.
	if rec, _ := e.call("", "", "GET", destPrefix, ""); rec.Code != 401 {
		t.Errorf("no auth: %d", rec.Code)
	}
	if rec, _ := e.call("erin", "ws-beta", "POST", destPrefix, createBody); rec.Code != 403 {
		t.Errorf("non-member: %d", rec.Code)
	}
}

func TestForeignAudienceAndDestination404(t *testing.T) {
	e := newDestEnv(t)
	// ws-beta's audience and destination are invisible to ws-alpha callers.
	code, m, _ := e.post("alice", "ws-alpha", `{"audience_id":"aud-beta-act","name":"x","destination_type":"salesforce"}`)
	if code != 404 || destDetail(m) != "Audience not found" {
		t.Errorf("foreign audience: %d %v", code, m)
	}
	code, m, _ = e.patchDest("alice", "ws-alpha", "dst-beta-crm", `{"name":"hijack"}`)
	if code != 404 || destDetail(m) != "Destination not found" {
		t.Errorf("foreign patch: %d %v", code, m)
	}
	rec, _ := e.call("alice", "ws-alpha", "DELETE", destPrefix+"/dst-beta-crm", "")
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "Destination not found") {
		t.Errorf("foreign delete: %d %s", rec.Code, rec.Body.String())
	}
	if n := destOwnerInt(t, `SELECT count(*) FROM audience_destinations WHERE id = 'dst-beta-crm' AND name <> 'hijack'`); n != 1 {
		t.Error("foreign destination changed or deleted")
	}
	if n := destOwnerInt(t, `SELECT count(*) FROM destination_inbound_tokens WHERE destination_id = 'dst-beta-crm' AND revoked_at IS NULL`); n == 0 {
		t.Error("foreign tokens revoked")
	}
	// The foreign audience filter lists nothing; the foreign owner sees it.
	rec, _ = e.call("alice", "ws-alpha", "GET", destPrefix+"?audience_id=aud-beta-act", "")
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("foreign filter: %s", rec.Body.String())
	}
	rec, _ = e.call("bob", "ws-beta", "GET", destPrefix, "")
	if !strings.Contains(rec.Body.String(), "dst-beta-crm") {
		t.Errorf("owner list: %s", rec.Body.String())
	}
	// Ids Postgres cannot hold match nothing.
	if rec, _ := e.call("alice", "ws-alpha", "PATCH", destPrefix+"/a%00b", `{"name":"x"}`); rec.Code != 404 {
		t.Errorf("NUL id: %d", rec.Code)
	}
	if rec, _ := e.call("alice", "ws-alpha", "DELETE", destPrefix+"/a%ffb", ""); rec.Code != 404 {
		t.Errorf("invalid utf8 id: %d", rec.Code)
	}
}

// S-M2 / S-M3: a destination secret reference must be in the outbound namespace, must not be a provider or CRM
// credential, and resolves to the workspace only. A refused name answers the same 422 as a missing one.
func TestOutboundRefPolicy(t *testing.T) {
	e := newDestEnv(t)
	ctx := context.Background()
	c := audOwner(t)
	for _, k := range []string{"HUBSPOT_API_KEY", "DEST_OK_TOKEN", "WEBHOOK_GLOBAL_ONLY"} {
		if k == "WEBHOOK_GLOBAL_ONLY" {
			_, err := c.Exec(ctx, `INSERT INTO app_settings (key, value) VALUES ($1, 'g') ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, k)
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		_, err := c.Exec(ctx, `INSERT INTO workspace_settings (workspace_id, key, value) VALUES ('ws-alpha', $1, 'v') ON CONFLICT (workspace_id, key) DO UPDATE SET value = 'v'`, k)
		if err != nil {
			t.Fatal(err)
		}
	}
	post := func(typ, cfg string) (int, string) {
		code, m, _ := e.post("alice", "ws-alpha", fmt.Sprintf(`{"audience_id":"aud-act-1","name":"p %s %d","destination_type":"%s","config":%s}`, typ, time.Now().UnixNano(), typ, cfg))
		return code, destDetail(m)
	}
	for _, tc := range []struct{ typ, cfg, want string }{
		{"webhook", `{"url":"https://h.example.test","header_secret_ref":"NO_SUCH"}`, "webhook header_secret_ref was not found"},
		{"webhook", `{"url":"https://h.example.test","header_secret_ref":"WEBHOOK_GLOBAL_ONLY"}`, "webhook header_secret_ref was not found"},
		{"webhook", `{"url":"https://h.example.test","header_secret_ref":"BETA_ONLY_SECRET"}`, "webhook header_secret_ref was not found"},
		{"warehouse_http", `{"url":"https://w.example.test","header_secret_ref":"HUBSPOT_API_KEY"}`, "warehouse_http header_secret_ref was not found"},
		{"slack", `{"webhook_secret_ref":"HUBSPOT_API_KEY"}`, "slack webhook_secret_ref was not found"},
	} {
		if code, d := post(tc.typ, tc.cfg); code != 422 || d != tc.want {
			t.Errorf("%s %s: %d %q", tc.typ, tc.cfg, code, d)
		}
	}
	// An existing in-namespace workspace secret, and a webhook without any header ref, are accepted.
	for _, tc := range []struct{ typ, cfg string }{
		{"webhook", `{"url":"https://h.example.test","header_secret_ref":"DEST_OK_TOKEN"}`},
		{"webhook", `{"url":"https://h.example.test"}`},
		{"warehouse_http", `{"url":"https://w.example.test","header_secret_ref":"DEST_OK_TOKEN"}`},
	} {
		if code, d := post(tc.typ, tc.cfg); code != 201 {
			t.Errorf("%s %s: %d %q", tc.typ, tc.cfg, code, d)
		}
	}
	// PATCH re-checks a webhook reference.
	code, m, _ := e.post("alice", "ws-alpha", `{"audience_id":"aud-act-1","name":"patchme","destination_type":"webhook","config":{"url":"https://h.example.test"}}`)
	if code != 201 {
		t.Fatalf("create: %d", code)
	}
	id, _ := m["id"].(string)
	if code, m, _ := e.patchDest("alice", "ws-alpha", id, `{"config":{"url":"https://h.example.test","header_secret_ref":"NO_SUCH"}}`); code != 422 || destDetail(m) != "webhook header_secret_ref was not found" {
		t.Errorf("patch: %d %v", code, m)
	}
}
