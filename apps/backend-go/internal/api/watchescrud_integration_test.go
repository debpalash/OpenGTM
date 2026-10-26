//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// Fixture order of testdata/contract/watches (legacy/python/scripts/contract_cases/automations.yaml, area
// watches, poll/signals excluded: Task 20). State mutates top to bottom, so every replay runs the whole
// sequence of its profile and only asserts the cases its test selects.
var wcrudOrder = strings.Fields(`
list_default list_limit_5 list_limit_1 list_limit_500 list_limit_0 list_limit_501 list_limit_text
list_offset_4_limit_4 list_offset_past_end list_offset_negative list_cursor_p1 list_cursor_p2
list_cursor_p3 list_cursor_ignores_offset list_cursor_exact_fit list_cursor_not_base64
list_cursor_not_json list_cursor_wrong_shape list_cursor_bad_id_type list_cursor_bad_date
list_cursor_naive_date list_cursor_too_long list_cursor_empty_ignored list_beta_bob
list_member_bob_alpha list_nonmember list_global_admin_other_workspace list_no_auth get_funding
get_hiring get_feed get_company_fresh get_job_change get_account_group_degraded get_disabled
get_failing get_auto_disabled get_account_group_paused get_account_group_healthy get_rate_limited
get_beta_own get_unknown get_foreign_ws get_no_auth create_funding_defaults create_funding_sec_cik
create_hiring create_feed create_company create_job_change_defaults create_job_change_weekly_max
create_invalid_kind create_account_group_refused create_missing_kind create_missing_target
create_empty_target create_blank_target_spaces create_invalid_interval create_interval_minutely
create_signal_types_wrong_kind create_signal_types_funding_hiring_mismatch
create_signal_types_empty_uses_all create_signal_types_company_all
create_signal_types_job_change_wrong create_lead_id_text create_lead_id_foreign_workspace
create_duplicate_target create_job_change_interval_daily create_job_change_interval_hourly
create_job_change_no_config create_job_change_empty_contacts create_job_change_contacts_not_list
create_job_change_contact_not_object create_job_change_lead_id_text create_job_change_lead_id_list
create_job_change_no_name_no_lead create_job_change_bad_linkedin
create_job_change_linkedin_company_page_rejected create_job_change_linkedin_ok_mixed_case
create_job_change_max_contacts_text create_job_change_max_contacts_zero
create_job_change_max_contacts_null_ignored create_job_change_max_contacts_float
create_job_change_config_not_object create_config_ignored_for_funding create_webhook_rule_ok
create_webhook_rule_no_secret create_webhook_rule_url_missing create_webhook_rule_url_blank
create_webhook_rule_bad_scheme create_webhook_rule_credentials create_webhook_rule_secret_missing
create_webhook_rule_loopback_passes create_webhook_fields_without_flag_ignored
create_member_forbidden create_member_invalid_body_403_first create_editor_ok create_nonmember
create_no_auth create_beta_owner create_no_body get_created_funding get_created_job_change
patch_interval patch_interval_invalid patch_signal_types patch_signal_types_empty_list_stored
patch_signal_types_invalid patch_lead_id patch_lead_id_foreign_workspace patch_lead_id_text
patch_config_unsupported_kind patch_disable patch_enable patch_enable_already_enabled patch_empty
patch_no_body patch_null_fields_noop patch_enabled_text patch_job_change_config
patch_job_change_config_invalid patch_job_change_config_empty_dict patch_job_change_interval_daily
patch_job_change_interval_weekly patch_signal_types_job_change
patch_account_group_config_unsupported patch_account_group_signal_types_subset
patch_account_group_signal_types_invalid patch_enable_disabled_seeded
patch_enable_auto_disabled_resets_failures patch_disable_account_group_state_paused
patch_enable_account_group_degraded_reset patch_disable_failing_keeps_error patch_unknown
patch_foreign_ws patch_member_forbidden patch_editor_ok patch_no_auth delete_created get_deleted
delete_again delete_seeded delete_unknown delete_foreign_ws delete_member_forbidden delete_editor_ok
delete_no_auth`)

var wcrudProfiles = map[string][]string{
	"off": strings.Fields(`off_watches_list off_watches_get off_watches_create off_watches_patch off_watches_delete
		off_watches_get_unknown off_watches_create_invalid_body_422_first off_watches_create_member_403_first
		off_watches_list_no_auth off_watches_list_bad_limit_422_first`),
	"poller_only": strings.Fields(`pol_watches_list_ok pol_watches_get_ok pol_create_webhook_rule_conflict
		pol_create_webhook_rule_conflict_before_url_check pol_create_no_rule_ok pol_create_webhook_rule_validation_first`),
	"nopg": strings.Fields(`nopg_list nopg_get nopg_create nopg_create_invalid_body_422_first nopg_patch nopg_delete
		nopg_member_403_first`),
	"limits": strings.Fields(`lim_watches_max_watches_alpha lim_watches_max_watches_beta lim_watches_validation_before_max
		lim_watches_job_change_too_many_contacts lim_watches_job_change_two_contacts_then_max lim_watches_list_still_works`),
}

const wcrudHookSecret = "ctk_automation_hook_secret_0001"

// wcrudActor is who sent a recorded request (the fixtures redact Authorization and X-Workspace-Id).
func wcrudActor(name string) (who, ws string) {
	switch name {
	case "list_member_bob_alpha", "create_member_forbidden", "create_member_invalid_body_403_first",
		"patch_member_forbidden", "delete_member_forbidden", "off_watches_create_member_403_first", "nopg_member_403_first":
		return "bob", "ws-alpha"
	case "list_beta_bob", "get_beta_own", "create_beta_owner", "lim_watches_max_watches_beta":
		return "bob", "ws-beta"
	case "list_nonmember", "create_nonmember":
		return "erin", "ws-beta"
	case "create_editor_ok", "patch_editor_ok", "delete_editor_ok":
		return "erin", ""
	case "list_global_admin_other_workspace":
		return "alice", "ws-beta"
	}
	if strings.HasSuffix(name, "_no_auth") {
		return "", ""
	}
	return "alice", ""
}

func wcrudFlags(profile string) config.Flags {
	f := config.Flags{PGLeadStore: true, IntentPollerEnabled: true, AutomationsEnabled: true}
	f.Automations = config.AutomationsSettings{GoRoutesEnabled: true, PollerDefaultInterval: "daily",
		PollerMaxWatchesPerWS: 200, PollerJobChangeMaxContacts: 500, PollerPollNowDailyQuota: 50, PollerPollNowMinIntervalSec: 60,
		MaxRulesPerWS: 50, MaxActionsPerRule: 10}
	switch profile {
	case "off":
		f.IntentPollerEnabled, f.AutomationsEnabled = false, false
	case "poller_only":
		f.AutomationsEnabled = false
	case "nopg":
		f.AutomationsEnabled, f.PGLeadStore = false, false
	case "limits":
		f.Automations.PollerMaxWatchesPerWS, f.Automations.PollerJobChangeMaxContacts = 2, 2
	}
	return f
}

type wcrudEnv struct {
	leadsEnv
	deps Deps
}

// newWcrudEnv seeds the contract users, workspaces, leads (newLeadsEnv), erin as ws-alpha editor, the
// recorded watches / schedules / poll ledger rows (testdata/watches/seed.json, dumped from the contract
// seed) and ws-alpha's AUTOMATION_HOOK_SECRET, then serves the watches CRUD routes.
func newWcrudEnv(t *testing.T, profile string, mod ...func(*Deps)) wcrudEnv {
	t.Helper()
	e := newLeadsEnv(t)
	e.seed(t, "erin", pythonHash, true, "user", false)
	e.member(t, "ws-alpha", e.mustUserByName(t, "erin").ID, "editor")
	// FastAPI makes a global admin an owner of every workspace at login (the recorded token_alice call),
	// which is why list_global_admin_other_workspace answers 200.
	e.member(t, "ws-beta", e.mustUserByName(t, "alice").ID, "owner")
	purge := func() {
		for _, q := range []string{`DELETE FROM watch_schedules`, `DELETE FROM watch_subscriptions`, `DELETE FROM poll_budget_ledger`,
			`DELETE FROM jobs WHERE type = 'watch_poll'`, `DELETE FROM scheduled_triggers`,
			`DELETE FROM triggers WHERE workspace_id IN ('ws-alpha','ws-beta')`,
			`DELETE FROM workspace_settings WHERE key = 'AUTOMATION_HOOK_SECRET'`} {
			wcrudOwnerExecQuiet(q)
		}
	}
	purge()
	t.Cleanup(purge)
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "watches", "seed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var seed map[string]map[string]struct {
		Columns []string          `json:"columns"`
		Rows    []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(raw, &seed); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"watch_subscriptions", "watch_schedules", "poll_budget_ledger"} {
		for _, ws := range []string{"ws-alpha", "ws-beta"} {
			s := seed[ws][tbl]
			if len(s.Rows) == 0 {
				continue
			}
			rows, _ := json.Marshal(s.Rows)
			cols := strings.Join(s.Columns, ", ")
			ownerExec(t, "INSERT INTO "+tbl+" ("+cols+") SELECT "+cols+" FROM json_populate_recordset(null::"+tbl+", $1::json)", string(rows))
		}
	}
	envl, err := secrets.NewEnvelope(secrets.Config{MasterKey: "watch-crud-test-key", AppEnv: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Pool: e.pool, Signer: testSigner, Identity: e.st, Flags: wcrudFlags(profile), Envelope: envl}
	if err := secrets.NewResolver(envl, secrets.NewPgStore(e.pool), os.Getenv).Set(context.Background(), "ws-alpha", "AUTOMATION_HOOK_SECRET", wcrudHookSecret); err != nil {
		t.Fatal(err)
	}
	for _, m := range mod {
		m(&d)
	}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(e.pool, nil))
	registerWatchesCRUD(r, d) // not RegisterWatches: the nopg profile must still answer 409 per request
	e.h = r.Handler()
	return wcrudEnv{leadsEnv: e, deps: d}
}

// wcrudOwnerExecQuiet runs best-effort cleanup SQL as the table owner (ignores errors).
func wcrudOwnerExecQuiet(sql string) {
	ctx := context.Background()
	c, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		return
	}
	defer c.Close(ctx)
	_, _ = c.Exec(ctx, sql)
}

func (e wcrudEnv) call(who, ws, method, path, body string) *httptest.ResponseRecorder {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
	}
	if ws != "" {
		hdr["X-Workspace-Id"] = ws
	}
	ct := ""
	if body != "" {
		ct = "application/json"
	}
	rec, _ := e.do(method, path, ct, body, hdr)
	return rec
}

func (e wcrudEnv) as(who, method, path, body string) (int, map[string]any, string) {
	ws := ""
	if who == "bob" {
		ws = "ws-beta" // bob's own workspace (his default is the ws-alpha membership)
	}
	rec := e.call(who, ws, method, path, body)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m, rec.Body.String()
}

func (e wcrudEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := wcrudOwnerQueryRow(t, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// wcrudOwnerQueryRow runs a read as the table owner (superuser: no row-level security), so a test sees every
// workspace's rows. Returns a scanner over a single-row result.
func wcrudOwnerQueryRow(t *testing.T, sql string, args ...any) pgx.Row {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(ctx) })
	return c.QueryRow(ctx, sql, args...)
}

const wcrudSampleTS = "2099-01-01T00:00:00.000001+00:00"

var wcrudNormUUID = regexp.MustCompile(`00000000-0000-4000-8000-\d{12}`)

// wcrudVolatilePaths lists the paths whose recorded value is the contract_normalize sample (generated id or
// timestamp): ids are ignored, timestamps keep their JSON type.
func wcrudVolatilePaths(body json.RawMessage) (ids, ts []string) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, nil
	}
	var walk func(p string, v any)
	walk = func(p string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				walk(p+"."+k, c)
			}
		case []any:
			for i, c := range x {
				walk(p+"["+strconv.Itoa(i)+"]", c)
			}
		case string:
			switch {
			case x == wcrudSampleTS:
				ts = append(ts, p)
			case wcrudNormUUID.MatchString(x):
				ids = append(ids, p)
			}
		}
	}
	walk("$", v)
	return ids, ts
}

// wcrudDeviations are the recorded cases where Go answers differently from FastAPI on purpose (D4).
var wcrudDeviations = map[string]struct {
	status int
	detail string
}{
	"create_duplicate_target": {409, "watch already exists for this kind and target"},
}

// replay sends every case of order against the routes in sequence; assert picks the ones compared.
func (e wcrudEnv) replay(t *testing.T, order []string, assert func(name string) bool) {
	t.Helper()
	ids := map[string]string{} // recorded (normalised) id -> real id
	for _, name := range order {
		fx := contract.Load(t, "watches/"+name)
		who, ws := wcrudActor(name)
		path := wcrudNormUUID.ReplaceAllStringFunc(fx.Request.Path, func(s string) string {
			if r, ok := ids[s]; ok {
				return r
			}
			return s
		})
		body := ""
		if b := strings.TrimSpace(string(fx.Request.Body)); b != "" && b != "null" {
			body = b
		}
		rec := e.call(who, ws, fx.Request.Method, path, body)
		var gotBody, wantBody map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &gotBody)
		_ = json.Unmarshal(fx.Response.Body, &wantBody)
		if rec.Code == 200 && fx.Request.Method == "POST" {
			w, _ := wantBody["id"].(string)
			g, _ := gotBody["id"].(string)
			if w != "" && g != "" {
				ids[w] = g
			}
		}
		if !assert(name) {
			continue
		}
		t.Run(name, func(t *testing.T) {
			got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: json.RawMessage(rec.Body.Bytes())}
			for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
				if v := rec.Header().Get(k); v != "" {
					got.Headers[k] = v
				}
			}
			if d, ok := wcrudDeviations[name]; ok {
				if rec.Code != d.status || gotBody["detail"] != d.detail {
					t.Fatalf("deviation %s: got %d %s, want %d %q", name, rec.Code, rec.Body.String(), d.status, d.detail)
				}
				return
			}
			idPaths, tsPaths := wcrudVolatilePaths(fx.Response.Body)
			contract.AssertEqual(t, got, fx.Response, contract.IgnorePaths(idPaths...), contract.IgnoreShapeOf(tsPaths...))
		})
	}
}

func wcrudReplayed(count *int, pick func(string) bool) func(string) bool {
	return func(n string) bool {
		ok := pick(n)
		if ok {
			*count++
		}
		return ok
	}
}

func TestWatchCreateMatchesFixturesPerKind(t *testing.T) {
	e := newWcrudEnv(t, "default")
	n := 0
	e.replay(t, wcrudOrder, wcrudReplayed(&n, func(name string) bool {
		return strings.HasPrefix(name, "create_") && !strings.HasPrefix(name, "create_webhook")
	}))
	if n < 45 {
		t.Fatalf("only %d create fixtures wcrudReplayed", n)
	}
	// created rows survive per kind (hiring and one feed are deleted again later in the recorded sequence)
	for _, k := range []string{"funding", "feed", "company", "job_change"} {
		if e.count(t, `SELECT count(*) FROM watch_subscriptions WHERE kind = $1 AND id NOT LIKE 'watch-%'`, k) < 1 {
			t.Errorf("no %s watch created", k)
		}
	}
}

func TestWatchCreateValidationStrings(t *testing.T) {
	e := newWcrudEnv(t, "default")
	for _, c := range []struct{ body, detail string }{
		{`{"kind":"nope","target":"x"}`, "invalid kind 'nope'"},
		{`{"kind":"account_group","target":"x"}`, "account_group watches require an exact persisted workbook selection"},
		{`{"kind":"funding","target":"x","interval":"minutely"}`, "invalid interval 'minutely'"},
		{`{"kind":"job_change","target":"x","interval":"daily","config":{"contacts":[{"lead_id":1}]}}`, "job_change watches poll at most weekly (interval='weekly')"},
		{`{"kind":"job_change","target":"x"}`, "job_change watch requires config.contacts (non-empty list)"},
		{`{"kind":"feed","target":"x","signal_types":["company_funded","news","bogus"]}`, "signal_types ['company_funded', 'bogus'] not valid for kind 'feed'"},
		{`{"kind":"funding","target":"x","create_webhook_rule":true}`, "webhook_url required when create_webhook_rule is set"},
	} {
		code, m, raw := e.as("alice", "POST", "/api/watches", c.body)
		if code != 422 || m["detail"] != c.detail {
			t.Errorf("%s: %d %s, want 422 %q", c.body, code, raw, c.detail)
		}
	}
	// pydantic envelope: every bad field is reported, in field order
	code, _, raw := e.as("alice", "POST", "/api/watches", `{"kind":5,"target":"","lead_id":"x","signal_types":"a","config":[1],"create_webhook_rule":"maybe"}`)
	last := -1
	for _, w := range []string{`"string_type"`, `"string_too_short"`, `"int_parsing"`, `"list_type"`, `"dict_type"`, `"bool_parsing"`} {
		i := strings.Index(raw, w)
		if code != 422 || i <= last {
			t.Fatalf("422 field errors out of order or missing %s: %d %s", w, code, raw)
		}
		last = i
	}
	if _, _, raw := e.as("alice", "POST", "/api/watches", `{"kind":"funding","target":"x","signal_types":["company_funded",3]}`); !strings.Contains(raw, `"loc":["body","signal_types",1]`) {
		t.Errorf("list element loc: %s", raw)
	}
	if e.count(t, `SELECT count(*) FROM watch_subscriptions WHERE target = 'x'`) != 0 {
		t.Error("a refused create left a row")
	}
}

func TestWatchCreateDuplicateIs409(t *testing.T) {
	e := newWcrudEnv(t, "default")
	body := `{"kind":"funding","target":"Dup Corp"}`
	if code, _, raw := e.as("alice", "POST", "/api/watches", body); code != 200 {
		t.Fatalf("first create: %d %s", code, raw)
	}
	code, m, raw := e.as("alice", "POST", "/api/watches", `{"kind":"funding","target":"  Dup Corp  "}`)
	if code != 409 || m["detail"] != "watch already exists for this kind and target" {
		t.Fatalf("duplicate (target trimmed): %d %s", code, raw)
	}
	if n := e.count(t, `SELECT count(*) FROM watch_subscriptions WHERE target = 'Dup Corp'`); n != 1 {
		t.Fatalf("%d rows for the duplicated target", n)
	}
	// the same target under another kind or in another workspace is not a duplicate
	if code, _, raw := e.as("alice", "POST", "/api/watches", `{"kind":"hiring","target":"Dup Corp"}`); code != 200 {
		t.Errorf("other kind: %d %s", code, raw)
	}
	if code, _, raw := e.as("bob", "POST", "/api/watches", body); code != 200 {
		t.Errorf("other workspace: %d %s", code, raw)
	}
}

func TestWatchCreateWithWebhookRule(t *testing.T) {
	e := newWcrudEnv(t, "default")
	n := 0
	e.replay(t, wcrudOrder, wcrudReplayed(&n, func(name string) bool { return strings.HasPrefix(name, "create_webhook") }))
	if n != 9 {
		t.Fatalf("%d webhook-rule fixtures wcrudReplayed", n)
	}
	code, m, raw := e.as("alice", "POST", "/api/watches",
		`{"kind":"feed","target":"https://feeds.example.com/rule.xml","create_webhook_rule":true,"webhook_url":"https://hooks.example.com/rule","webhook_secret_ref":"AUTOMATION_HOOK_SECRET"}`)
	if code != 200 || m["webhook_rule_id"] == nil {
		t.Fatalf("%d %s", code, raw)
	}
	if strings.Contains(raw, wcrudHookSecret) {
		t.Fatal("secret value echoed in the create response")
	}
	var typ, actions, tcfg string
	if err := wcrudOwnerQueryRow(t, `SELECT trigger_type, actions::text, trigger_config::text FROM triggers WHERE id = $1`,
		m["webhook_rule_id"]).Scan(&typ, &actions, &tcfg); err != nil {
		t.Fatal(err)
	}
	if typ != "on_signal" || !strings.Contains(actions, `"header_secret_ref"`) || strings.Contains(actions, wcrudHookSecret) || !strings.Contains(tcfg, "news") {
		t.Errorf("rule %s %s %s", typ, actions, tcfg)
	}
	// an unknown or foreign-workspace secret is 422 and leaves no rule behind
	before := e.count(t, `SELECT count(*) FROM triggers`)
	for who, ref := range map[string]string{"alice": "NOPE", "bob": "AUTOMATION_HOOK_SECRET"} {
		body := `{"kind":"feed","target":"https://feeds.example.com/` + who + `.xml","create_webhook_rule":true,"webhook_url":"https://hooks.example.com/x","webhook_secret_ref":"` + ref + `"}`
		if code, m, _ := e.as(who, "POST", "/api/watches", body); code != 422 || m["detail"] != "webhook_secret_ref not found" {
			t.Errorf("%s secret %s: %d %v", who, ref, code, m)
		}
	}
	if after := e.count(t, `SELECT count(*) FROM triggers`); after != before {
		t.Errorf("refused creates left %d orphan rules", after-before)
	}
	// a webhook host outside AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST is refused (Go hardening)
}

// TestWatchCreateWebhookAllowlist: a webhook host outside AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST is refused
// (Go hardening) and leaves no rule behind.
func TestWatchCreateWebhookAllowlist(t *testing.T) {
	e := newWcrudEnv(t, "default", func(d *Deps) { d.Flags.Automations.WebhookDomainAllowlist = []string{"hooks.example.com"} })
	before := e.count(t, `SELECT count(*) FROM triggers`)
	if code, _, raw := e.as("alice", "POST", "/api/watches",
		`{"kind":"feed","target":"https://feeds.example.com/al.xml","create_webhook_rule":true,"webhook_url":"https://evil.example.net/x"}`); code != 422 {
		t.Errorf("allow-list miss: %d %s", code, raw)
	}
	if code, _, raw := e.as("alice", "POST", "/api/watches",
		`{"kind":"feed","target":"https://feeds.example.com/ok.xml","create_webhook_rule":true,"webhook_url":"https://hooks.example.com/x"}`); code != 200 {
		t.Errorf("allow-listed host: %d %s", code, raw)
	}
	if after := e.count(t, `SELECT count(*) FROM triggers`); after != before+1 {
		t.Errorf("%d rules created, want 1", after-before)
	}
}

func TestWatchListPaginationMatchesFixtures(t *testing.T) {
	e := newWcrudEnv(t, "default")
	n := 0
	bad := map[string]bool{"list_cursor_not_base64": true, "list_cursor_not_json": true, "list_cursor_wrong_shape": true,
		"list_cursor_bad_id_type": true, "list_cursor_bad_date": true, "list_cursor_too_long": true,
		"list_limit_0": true, "list_limit_501": true, "list_limit_text": true, "list_offset_negative": true}
	e.replay(t, wcrudOrder, wcrudReplayed(&n, func(name string) bool { return strings.HasPrefix(name, "list_") && !bad[name] }))
	if n != 18 {
		t.Fatalf("%d list fixtures wcrudReplayed", n)
	}
	_, m, raw := e.as("alice", "GET", "/api/watches?limit=5", "")
	if m["has_more"] != true || m["next_cursor"] == nil {
		t.Fatalf("page 1: %s", raw)
	}
	if !strings.HasPrefix(raw, `{"watches":[`) || !strings.Contains(raw, `],"limit":5,"offset":0,"has_more":true,"next_cursor":"`) {
		t.Errorf("key order: %.120s", raw)
	}
	_, m2, raw2 := e.as("alice", "GET", "/api/watches?limit=5&cursor="+m["next_cursor"].(string), "")
	if v, present := m2["offset"]; !present || v != nil || m2["limit"] != float64(5) {
		t.Fatalf("cursor page must carry offset null: %s", raw2)
	}
}

func TestWatchListInvalidCursor422(t *testing.T) {
	e := newWcrudEnv(t, "default")
	bad := map[string]bool{"list_cursor_not_base64": true, "list_cursor_not_json": true, "list_cursor_wrong_shape": true,
		"list_cursor_bad_id_type": true, "list_cursor_bad_date": true, "list_cursor_too_long": true,
		"list_limit_0": true, "list_limit_501": true, "list_limit_text": true, "list_offset_negative": true}
	n := 0
	e.replay(t, wcrudOrder, wcrudReplayed(&n, func(name string) bool { return bad[name] }))
	if n != 10 {
		t.Fatalf("%d invalid cursor/limit fixtures wcrudReplayed", n)
	}
	// every query error is reported, limit first
	code, _, raw := e.as("alice", "GET", "/api/watches?limit=0&offset=-1", "")
	if code != 422 || strings.Index(raw, `"limit"`) > strings.Index(raw, `"offset"`) || strings.Count(raw, `"loc"`) != 2 {
		t.Errorf("two query errors: %d %s", code, raw)
	}
}

func TestWatchGetMatchesFixture(t *testing.T) {
	e := newWcrudEnv(t, "default")
	n := 0
	e.replay(t, wcrudOrder, wcrudReplayed(&n, func(name string) bool { return strings.HasPrefix(name, "get_") && name != "get_deleted" }))
	if n != 18 {
		t.Fatalf("%d get fixtures wcrudReplayed", n)
	}
}

func TestWatchPatchMatchesFixtures(t *testing.T) {
	e := newWcrudEnv(t, "default")
	n := 0
	e.replay(t, wcrudOrder, wcrudReplayed(&n, func(name string) bool { return strings.HasPrefix(name, "patch_") }))
	if n != 35 {
		t.Fatalf("%d patch fixtures wcrudReplayed", n)
	}
}

func TestWatchDeleteMatchesFixture(t *testing.T) {
	e := newWcrudEnv(t, "default")
	n := 0
	e.replay(t, wcrudOrder, wcrudReplayed(&n, func(name string) bool { return strings.HasPrefix(name, "delete_") || name == "get_deleted" }))
	if n != 9 {
		t.Fatalf("%d delete fixtures wcrudReplayed", n)
	}
	if c := e.count(t, `SELECT count(*) FROM watch_schedules WHERE watch_id = 'watch-a-08'`); c != 0 {
		t.Errorf("%d mirror rows left for a deleted watch", c)
	}
	code, _, raw := e.as("alice", "DELETE", "/api/watches/watch-a-01", "")
	if code != 200 || raw != `{"deleted":true,"id":"watch-a-01"}` {
		t.Errorf("delete body: %d %s", code, raw)
	}
}

func TestWatchEditorRequiredForMutations(t *testing.T) {
	e := newWcrudEnv(t, "default")
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/watches", `{"kind":"funding","target":"x"}`},
		{"PATCH", "/api/watches/watch-a-01", `{"enabled":false}`},
		{"DELETE", "/api/watches/watch-a-01", ""},
	} {
		rec := e.call("bob", "ws-alpha", c.method, c.path, c.body)
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "Workspace permission denied: signals.write") {
			t.Errorf("member %s: %d %s", c.method, rec.Code, rec.Body.String())
		}
		if rec := e.call("", "", c.method, c.path, c.body); rec.Code != 401 {
			t.Errorf("anonymous %s: %d", c.method, rec.Code)
		}
	}
	for _, p := range []string{"/api/watches", "/api/watches/watch-a-01"} {
		if rec := e.call("bob", "ws-alpha", "GET", p, ""); rec.Code != 200 {
			t.Errorf("member read %s: %d", p, rec.Code)
		}
	}
	if e.count(t, `SELECT count(*) FROM watch_subscriptions WHERE id = 'watch-a-01' AND enabled`) != 1 {
		t.Error("a forbidden patch changed the watch")
	}
}

func TestWatchFlagGating404Then409(t *testing.T) {
	for _, p := range []string{"off", "poller_only", "nopg", "limits"} {
		t.Run(p, func(t *testing.T) {
			e := newWcrudEnv(t, p)
			e.replay(t, wcrudProfiles[p], func(string) bool { return true })
		})
	}
	t.Run("both off", func(t *testing.T) {
		// the poller 404 wins over the lead-store 409
		e := newWcrudEnv(t, "default", func(d *Deps) { d.Flags.IntentPollerEnabled, d.Flags.PGLeadStore = false, false })
		if code, m, _ := e.as("alice", "GET", "/api/watches", ""); code != 404 || m["detail"] != "intent poller disabled" {
			t.Errorf("both off: %d %v", code, m)
		}
	})
	t.Run("no pg store", func(t *testing.T) {
		e := newWcrudEnv(t, "default", func(d *Deps) { d.Flags.PGLeadStore = false })
		if code, m, _ := e.as("alice", "DELETE", "/api/watches/watch-a-01", ""); code != 409 || m["detail"] != "intent_poller_requires_pg_lead_store" {
			t.Errorf("no pg: %d %v", code, m)
		}
		if e.count(t, `SELECT count(*) FROM watch_subscriptions WHERE id = 'watch-a-01'`) != 1 {
			t.Error("a gated delete removed the watch")
		}
	})
}

func TestWatchForeignWatch404(t *testing.T) {
	e := newWcrudEnv(t, "default")
	for _, c := range []struct{ method, body string }{{"GET", ""}, {"PATCH", `{"enabled":false}`}, {"DELETE", ""}} {
		code, m, raw := e.as("alice", c.method, "/api/watches/watch-b-01", c.body)
		if code != 404 || m["detail"] != "watch not found" {
			t.Errorf("%s foreign: %d %s", c.method, code, raw)
		}
		if code, _, _ := e.as("alice", c.method, "/api/watches/no-such-watch", c.body); code != 404 {
			t.Errorf("%s unknown: %d", c.method, code)
		}
	}
	if e.count(t, `SELECT count(*) FROM watch_subscriptions WHERE id = 'watch-b-01' AND enabled`) != 1 {
		t.Error("a cross-workspace mutation changed the foreign watch")
	}
	if e.count(t, `SELECT count(*) FROM watch_schedules WHERE watch_id = 'watch-b-01'`) != 1 {
		t.Error("a cross-workspace delete removed the foreign schedule mirror")
	}
	if _, _, raw := e.as("alice", "GET", "/api/watches?limit=500", ""); strings.Contains(raw, "watch-b-0") {
		t.Error("ws-beta watch in the ws-alpha list")
	}
}

func TestWatchForeignLeadPin422(t *testing.T) {
	e := newWcrudEnv(t, "default")
	code, m, raw := e.as("alice", "POST", "/api/watches", `{"kind":"funding","target":"Pin Co","lead_id":17}`)
	if code != 422 || m["detail"] != "lead_id 17 not found in this workspace" {
		t.Fatalf("create: %d %s", code, raw)
	}
	if e.count(t, `SELECT count(*) FROM watch_subscriptions WHERE target = 'Pin Co'`) != 0 {
		t.Error("a refused create left a row")
	}
	for _, id := range []string{"17", "18", "999999", "99999999999"} {
		code, m, raw = e.as("alice", "PATCH", "/api/watches/watch-a-02", `{"lead_id":`+id+`}`)
		if d, _ := m["detail"].(string); code != 422 || !strings.HasSuffix(d, "not found in this workspace") {
			t.Errorf("patch lead %s: %d %s", id, code, raw)
		}
	}
	if e.count(t, `SELECT count(*) FROM watch_subscriptions WHERE id = 'watch-a-02' AND lead_id = 14`) != 1 {
		t.Error("a refused patch changed the lead pin")
	}
	if code, _, raw := e.as("alice", "PATCH", "/api/watches/watch-a-02", `{"lead_id":6}`); code != 200 {
		t.Errorf("own lead: %d %s", code, raw)
	}
}

func TestWatchAccountGroupExtrasInResponse(t *testing.T) {
	e := newWcrudEnv(t, "default")
	_, m, raw := e.as("alice", "GET", "/api/watches/watch-a-06", "")
	if m["target"] != "Automation Matrix" || m["state"] != "degraded" || m["account_count"] != float64(3) || m["attempt_count"] != float64(3) {
		t.Fatalf("%s", raw)
	}
	// Python's dict order: base fields, then the account_group extras (target keeps its first position)
	keys := []string{"id", "workspace_id", "kind", "target", "resolved_cik", "lead_id", "signal_types", "interval", "config", "enabled",
		"next_poll_at", "last_polled_at", "last_error", "consecutive_failures", "cursor", "created_at", "workbook_id", "scope_key",
		"account_count", "account_ids", "state", "attempt_count", "last_error_class", "next_retry_at", "collector_health", "manual_retry_action"}
	got := wcrudTopKeys(t, raw)
	if strings.Join(got, ",") != strings.Join(keys, ",") {
		t.Fatalf("key order\n got %v\nwant %v", got, keys)
	}
	// a plain watch has no extras and no webhook_rule_id on read; create appends webhook_rule_id last
	if _, _, plain := e.as("alice", "GET", "/api/watches/watch-a-01", ""); strings.Contains(plain, `"account_count"`) || strings.Contains(plain, `"webhook_rule_id"`) {
		t.Errorf("plain watch: %s", plain)
	}
	if _, _, created := e.as("alice", "POST", "/api/watches", `{"kind":"feed","target":"https://feeds.example.com/order.xml"}`); !strings.HasSuffix(created, `,"webhook_rule_id":null}`) {
		t.Errorf("create tail: %s", created)
	}
	// patching an account group keeps the extras; the state follows enabled
	_, m, raw = e.as("alice", "PATCH", "/api/watches/watch-a-06", `{"enabled":false}`)
	if m["state"] != "paused" || m["next_retry_at"] != nil || m["account_count"] != float64(3) {
		t.Errorf("paused group: %s", raw)
	}
}

func TestWatchCreateEnqueuesPollInConfiguredExecutor(t *testing.T) {
	e := newWcrudEnv(t, "default", func(d *Deps) {
		d.Flags.Automations.GoExecutorWatchPoll = true
		d.Getenv = func(k string) string {
			switch k {
			case "GO_EXECUTOR_ENABLED":
				return "true"
			case "GO_EXECUTOR_WORKSPACES":
				return "ws-alpha"
			}
			return ""
		}
	})
	for who, want := range map[string]string{"alice": "go", "bob": "python"} {
		code, m, raw := e.as(who, "POST", "/api/watches", `{"kind":"feed","target":"https://feeds.example.com/exec.xml"}`)
		if code != 200 {
			t.Fatalf("%s: %d %s", who, code, raw)
		}
		var exec string
		if err := wcrudOwnerQueryRow(t, `SELECT executor FROM jobs WHERE type = 'watch_poll' AND payload->>'watch_id' = $1`, m["id"]).Scan(&exec); err != nil {
			t.Fatal(err)
		}
		if exec != want {
			t.Errorf("%s: job executor %q, want %s", who, exec, want)
		}
	}
}

// wcrudTopKeys returns the top-level keys of a JSON object in document order.
func wcrudTopKeys(t *testing.T, raw string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %v", err)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}
