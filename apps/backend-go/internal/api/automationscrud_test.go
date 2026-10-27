//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	autoFixtureArea = "automations"
	autoSecretName  = "AUTOMATION_HOOK_SECRET"
)

// autoDeviations are recorded cases where Python answers 500, or stores a value the Go port refuses (D4). They are not
// replayed against the fixture; the named deviation tests below pin the Go answer.
var autoDeviations = map[string]bool{
	"create_action_config_string": true, // Python 500
	"create_action_config_list":   true, // Python 500
	"patch_name_long_db_error":    true, // Python 500 (varchar(200) overflow)
	"create_negative_spend_cap":   true, // Python stores -1.0; Go 422 (D4)
	// WP1b: Python bugs fixed in Go (automationscrud_outbound_test.go covers the new behaviour).
	"create_webhook_templated_url":               true, // S-L1: templated authority refused
	"create_webhook_templated_bad_scheme_passes": true, // S-L1: scheme checked on the literal head
	"create_webhook_secret_ref_missing":          true, // S-M1: one shared 422 text
	"create_webhook_secret_ref_foreign_ws":       true, // S-M1: one shared 422 text
	"list_member_bob_alpha":                      true, // S-L3: members without automations.manage see masked webhook config
	"list_editor_erin":                           true, // S-L3
}

// autoClockValues are wall-clock values the recorder did not normalise: the next run of a seeded schedule rule after a
// pause or resume depends on the hour the fixtures were recorded.
var autoClockValues = map[string][]string{
	"get_after_pause_schedule":  {"$.next_run_at"},
	"get_after_resume_schedule": {"$.next_run_at"},
}

type autoEnv struct {
	mcpEnv
	profile string
}

type autoProfile struct {
	on                  bool
	maxRules, maxAction int
}

var autoProfiles = map[string]autoProfile{
	"default":     {on: true, maxRules: 50, maxAction: 10},
	"off":         {on: false, maxRules: 50, maxAction: 10},
	"poller_only": {on: false, maxRules: 50, maxAction: 10},
	"limits":      {on: true, maxRules: 3, maxAction: 2},
}

// newAutoEnv builds the router for one recording profile and loads the seed the fixtures were recorded against:
// users 1/2/4 (alice global admin and owner of ws-alpha, bob member of ws-alpha and owner of ws-beta, erin editor of
// ws-alpha), the dump of contract_seed.py CONTRACT_SEED_AUTOMATIONS=yes (legacy/python/scripts/dump_automation_seed.py)
// and the ws-alpha secret AUTOMATION_HOOK_SECRET.
func newAutoEnv(t *testing.T, profile string) autoEnv {
	t.Helper()
	p, ok := autoProfiles[profile]
	if !ok {
		t.Fatalf("unknown profile %q", profile)
	}
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{},
		Flags: config.Flags{PGLeadStore: true, AutomationsEnabled: p.on, Automations: config.AutomationsSettings{
			GoRoutesEnabled: true, MaxRulesPerWS: p.maxRules, MaxActionsPerRule: p.maxAction}}}
	RegisterAutomations(r, d)
	e := autoEnv{mcpEnv: mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}, profile: profile}
	e.purgeAuto()
	t.Cleanup(e.purgeAuto)
	e.seedAuto(t)
	return e
}

func (e autoEnv) purgeAuto() {
	ctx := context.Background()
	for _, ws := range []string{"ws-alpha", "ws-beta"} {
		_ = tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			for _, tbl := range []string{"trigger_action_results", "trigger_runs", "trigger_cap_reservations", "triggers",
				"workbook_rows", "workbooks", "workspace_settings", "governance_audit_events"} {
				_, _ = tx.Exec(ctx, "DELETE FROM "+tbl)
			}
			_, _ = tx.Exec(ctx, `DELETE FROM jobs WHERE workspace_id = $1`, ws)
			_, _ = tx.Exec(ctx, `DELETE FROM scheduled_triggers WHERE workspace_id = $1`, ws)
			return nil
		})
	}
}

func (e autoEnv) seedAuto(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `DELETE FROM users WHERE id IN (1,2,3,4) OR username IN ('alice','bob','carol','erin')`); err != nil {
		t.Fatal(err)
	}
	for _, u := range []struct {
		id          int
		name, role  string
		admin, live bool
	}{{1, "alice", "admin", true, true}, {2, "bob", "user", false, true}, {4, "erin", "user", false, true}} {
		if _, err := e.pool.Exec(ctx, `INSERT INTO users (id, username, hashed_password, is_active, is_admin, role) VALUES ($1,$2,$3,$4,$5,$6)`,
			u.id, u.name, pythonHash, u.live, u.admin, u.role); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM users WHERE id IN (1,2,4)`)
	})
	for _, ws := range []string{"ws-alpha", "ws-beta"} { // leftovers of an aborted run
		_, _ = e.pool.Exec(ctx, `DELETE FROM workspace_members WHERE workspace_id = $1`, ws)
		_, _ = e.pool.Exec(ctx, `DELETE FROM workspaces WHERE id = $1`, ws)
	}
	e.workspace(t, "ws-alpha", 1)
	e.workspace(t, "ws-beta", 2)
	t.Cleanup(e.purgeAuto) // runs before the workspaces are deleted (cleanups are LIFO)
	e.member(t, "ws-alpha", 1, "owner")
	e.member(t, "ws-alpha", 2, "member")
	e.member(t, "ws-beta", 2, "owner")
	// FastAPI makes a global admin an owner of every workspace at login (the recorded token_alice call), which is
	// why list_global_admin_other_workspace answers 200.
	e.member(t, "ws-beta", 1, "owner")
	e.member(t, "ws-alpha", 4, "editor")

	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "automations", "seed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var seed map[string]map[string]struct {
		Columns []string        `json:"columns"`
		Rows    json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(raw, &seed); err != nil {
		t.Fatal(err)
	}
	for ws, tables := range seed {
		err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			for _, tbl := range []string{"workbooks", "workbook_rows", "triggers", "trigger_runs", "trigger_action_results",
				"trigger_cap_reservations", "scheduled_triggers"} {
				d := tables[tbl]
				colNames := d.Columns
				if tbl == "trigger_cap_reservations" { // serial key shared with every workspace; nothing refers to it
					colNames = nil
					for _, c := range d.Columns {
						if c != "id" {
							colNames = append(colNames, c)
						}
					}
				}
				cols := strings.Join(colNames, ", ")
				if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %[1]s (%[2]s) SELECT %[2]s FROM json_populate_recordset(null::%[1]s, $1::json)`, tbl, cols), string(d.Rows)); err != nil {
					return fmt.Errorf("%s %s: %w", ws, tbl, err)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := secrets.NewPgStore(e.pool).SetWorkspaceSetting(ctx, "ws-alpha", autoSecretName, "test-only-not-a-secret"); err != nil {
		t.Fatal(err)
	}
}

var autoUUIDRE = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}`)

// autoReplay replays the CRUD cases of one profile in recording order. Created ids are random, so a created id in a
// later request path is rewritten to the real one and every response id is mapped back to the recorded one. check
// selects the cases whose response is asserted; the others still run, because they carry state.
type autoReplay struct {
	e       autoEnv
	toReal  map[string]string // recorded id -> real id
	toFixed map[string]string // real id -> recorded id
}

func (e autoEnv) send(who, ws, method, path, body string) (int, []byte, map[string]string) {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
		if ws != "" {
			hdr["X-Workspace-Id"] = ws
		}
	}
	ct := ""
	if body != "" {
		ct = "application/json"
	}
	rec, _ := e.do(method, path, ct, body, hdr)
	return rec.Code, rec.Body.Bytes(), autoPickHeaders(rec)
}

func autoPickHeaders(rec *httptest.ResponseRecorder) map[string]string {
	h := map[string]string{}
	for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
		if v := rec.Header().Get(k); v != "" {
			h[k] = v
		}
	}
	return h
}

func autoExpandVolatile(paths []string, want json.RawMessage) []string {
	var out []string
	var arr []json.RawMessage
	_ = json.Unmarshal(want, &arr)
	for _, p := range paths {
		if !strings.Contains(p, "[]") {
			out = append(out, p)
			continue
		}
		for i := range arr {
			out = append(out, strings.Replace(p, "[]", "["+strconv.Itoa(i)+"]", 1))
		}
	}
	return out
}

func (e autoEnv) replay(t *testing.T, check func(autoCase) bool) {
	t.Helper()
	rp := autoReplay{e: e, toReal: map[string]string{}, toFixed: map[string]string{}}
	for _, c := range autoCases {
		if c.profile != e.profile || autoDeviations[c.name] {
			continue
		}
		fx := contract.Load(t, autoFixtureArea+"/"+c.name)
		path := fx.Request.Path
		for fixed, real := range rp.toReal {
			path = strings.ReplaceAll(path, fixed, real)
		}
		body := ""
		if len(fx.Request.Body) > 0 && string(fx.Request.Body) != "null" {
			body = string(fx.Request.Body)
		}
		code, out, hdr := e.send(c.who, c.ws, fx.Request.Method, path, body)
		if code == 200 && fx.Request.Method == "POST" && strings.HasSuffix(fx.Request.Path, "/triggers") {
			var g, w struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(out, &g) == nil && json.Unmarshal(fx.Response.Body, &w) == nil && g.ID != "" && w.ID != "" {
				if !autoUUIDRE.MatchString(g.ID) {
					t.Errorf("%s: created id %q is not a uuid4", c.name, g.ID)
				}
				rp.toReal[w.ID], rp.toFixed[g.ID] = g.ID, w.ID
			}
		}
		if !check(c) {
			continue
		}
		text := string(out)
		for real, fixed := range rp.toFixed {
			text = strings.ReplaceAll(text, real, fixed)
		}
		got := contract.Resp{Status: code, Headers: hdr, Body: json.RawMessage(text)}
		t.Run(c.name, func(t *testing.T) {
			contract.AssertEqual(t, got, fx.Response, contract.IgnorePaths(autoExpandVolatile(append(append([]string{}, c.volatile...), autoClockValues[c.name]...), fx.Response.Body)...))
		})
	}
}

func autoHasAnyPrefix(name string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func autoRecordedStatus(t *testing.T, name string) int {
	return contract.Load(t, autoFixtureArea+"/"+name).Response.Status
}

func TestAutoCreateMatchesFixtures(t *testing.T) {
	e := newAutoEnv(t, "default")
	n := 0
	e.replay(t, func(c autoCase) bool {
		ok := strings.HasPrefix(c.name, "create_") && autoRecordedStatus(t, c.name) == 200
		if ok {
			n++
		}
		return ok
	})
	if n < 25 {
		t.Fatalf("only %d successful create fixtures replayed", n)
	}
}

func TestAutoCreateValidationStrings(t *testing.T) {
	e := newAutoEnv(t, "default")
	n := 0
	e.replay(t, func(c autoCase) bool {
		s := autoRecordedStatus(t, c.name)
		ok := strings.HasPrefix(c.name, "create_") && (s == 422 || s == 409)
		if ok {
			n++
		}
		return ok
	})
	if n < 46 { // 50 minus the two secret-ref fixtures deviated in WP1b (S-M1)
		t.Fatalf("only %d create validation fixtures replayed", n)
	}
}

func TestAutoCreateDisabledFlag404(t *testing.T) {
	for profile, min := range map[string]int{"off": 8, "poller_only": 2} {
		t.Run(profile, func(t *testing.T) {
			e := newAutoEnv(t, profile)
			n := 0
			e.replay(t, func(c autoCase) bool { n++; return true })
			if n < min {
				t.Fatalf("only %d %s fixtures replayed", n, profile)
			}
		})
	}
	// the flag answers 404 only after validation and the role gate, and never writes
	e := newAutoEnv(t, "off")
	code, body, _ := e.send("alice", "ws-alpha", "POST", "/api/automations/triggers", `{"name":"x","trigger_type":"on_row_added"}`)
	if code != 404 || !strings.Contains(string(body), `"automations disabled"`) {
		t.Fatalf("off create: %d %s", code, body)
	}
	if n := e.triggerCount(t, "ws-alpha"); n != 10 {
		t.Fatalf("disabled create wrote a rule: %d rules", n)
	}
}

func TestAutoCreateRequiresAdmin(t *testing.T) {
	e := newAutoEnv(t, "default")
	// editor and viewer cannot mutate anything (403), reads stay open to members
	for _, who := range []string{"erin", "bob"} {
		for _, rq := range [][2]string{{"POST", "/api/automations/triggers"}, {"PATCH", "/api/automations/triggers/trg-a-01"},
			{"DELETE", "/api/automations/triggers/trg-a-01"}, {"POST", "/api/automations/triggers/trg-a-01/pause"},
			{"POST", "/api/automations/triggers/trg-a-01/resume"}} {
			code, body, _ := e.send(who, "ws-alpha", rq[0], rq[1], `{"name":"x","trigger_type":"on_row_added"}`)
			if code != 403 || !strings.Contains(string(body), "Workspace permission denied: automations.manage") {
				t.Errorf("%s %s %s: %d %.200s", who, rq[0], rq[1], code, body)
			}
		}
		if code, _, _ := e.send(who, "ws-alpha", "GET", "/api/automations/triggers", ""); code != 200 {
			t.Errorf("%s list: %d", who, code)
		}
	}
	if n := e.triggerCount(t, "ws-alpha"); n != 10 {
		t.Fatalf("a forbidden call changed the rules: %d", n)
	}
	n := 0
	e.replay(t, func(c autoCase) bool {
		s := autoRecordedStatus(t, c.name)
		ok := (s == 401 || s == 403) && strings.HasPrefix(c.name, "create_")
		if ok {
			n++
		}
		return ok
	})
	if n < 5 {
		t.Fatalf("only %d create gate fixtures replayed", n)
	}
}

func TestAutoListAndFilters(t *testing.T) {
	e := newAutoEnv(t, "default")
	_, body, _ := e.send("alice", "ws-alpha", "GET", "/api/automations/triggers", "")
	var rules []struct {
		ID        string `json:"id"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(body, &rules); err != nil || len(rules) != 10 {
		t.Fatalf("list: %v %.200s", err, body)
	}
	for i := 1; i < len(rules); i++ {
		if rules[i-1].CreatedAt < rules[i].CreatedAt {
			t.Fatalf("list not ordered created_at desc: %s before %s", rules[i-1].CreatedAt, rules[i].CreatedAt)
		}
	}
	if _, empty, _ := e.send("alice", "ws-alpha", "GET", "/api/automations/triggers?trigger_type=nope", ""); strings.TrimSpace(string(empty)) != "[]" {
		t.Fatalf("empty list must be [] not null: %.100s", empty)
	}
	e.replay(t, func(c autoCase) bool { return strings.HasPrefix(c.name, "list_") })
}

func TestAutoGetWithRecentRuns(t *testing.T) {
	e := newAutoEnv(t, "default")
	_, body, _ := e.send("alice", "ws-alpha", "GET", "/api/automations/triggers/trg-a-01", "")
	var d struct {
		RecentRuns []struct {
			StartedAt string `json:"started_at"`
		} `json:"recent_runs"`
	}
	if err := json.Unmarshal(body, &d); err != nil || len(d.RecentRuns) != 10 {
		t.Fatalf("recent_runs: %v len=%d (12 seeded runs, last 10 wanted)", err, len(d.RecentRuns))
	}
	for i := 1; i < len(d.RecentRuns); i++ {
		if d.RecentRuns[i-1].StartedAt < d.RecentRuns[i].StartedAt {
			t.Fatal("recent_runs not newest first")
		}
	}
	e.replay(t, func(c autoCase) bool { return strings.HasPrefix(c.name, "get_") })
}

func TestAutoPatchMatchesFixtures(t *testing.T) {
	e := newAutoEnv(t, "default")
	n := 0
	e.replay(t, func(c autoCase) bool {
		ok := strings.HasPrefix(c.name, "patch_")
		if ok {
			n++
		}
		return ok
	})
	if n < 30 {
		t.Fatalf("only %d patch fixtures replayed", n)
	}
}

func TestAutoDeleteMatchesFixture(t *testing.T) {
	e := newAutoEnv(t, "default")
	e.replay(t, func(c autoCase) bool { return strings.HasPrefix(c.name, "delete_") || c.name == "get_deleted" })
	// deleting a schedule rule removes its mirror row; history rows of a deleted rule stay (recorded)
	var mirrors, runs int
	_ = tenancy.InWorkspace(context.Background(), e.pool, "ws-alpha", func(tx pgx.Tx) error {
		_ = tx.QueryRow(context.Background(), `SELECT count(*) FROM scheduled_triggers WHERE trigger_id = 'trg-a-06'`).Scan(&mirrors)
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM trigger_runs WHERE trigger_id = 'trg-a-01'`).Scan(&runs)
	})
	if mirrors != 0 || runs == 0 {
		t.Fatalf("mirror rows after delete %d (want 0), kept runs %d (want >0)", mirrors, runs)
	}
}

func TestAutoPauseResumeMatchFixtures(t *testing.T) {
	e := newAutoEnv(t, "default")
	n := 0
	e.replay(t, func(c autoCase) bool {
		ok := autoHasAnyPrefix(c.name, "pause_", "resume_", "get_after_")
		if ok {
			n++
		}
		return ok
	})
	if n < 15 {
		t.Fatalf("only %d pause/resume fixtures replayed", n)
	}
}

func TestAutoForeignTrigger404(t *testing.T) {
	e := newAutoEnv(t, "default")
	// opaque ids: anything that is not a rule of the caller's workspace is a plain 404, never 403 or 500
	for _, id := range []string{"trg-b-01", "does-not-exist", "%27%3B%20drop%20table%20triggers%3B--", "0", "%E2%82%AC", strings.Repeat("a", 300)} {
		for _, rq := range [][2]string{{"GET", ""}, {"PATCH", `{"name":"y"}`}, {"DELETE", ""}, {"POST", "/pause"}, {"POST", "/resume"}} {
			path := "/api/automations/triggers/" + id
			method, body := rq[0], rq[1]
			if method == "POST" {
				path += body
				body = ""
			}
			code, out, _ := e.send("alice", "ws-alpha", method, path, body)
			if code != 404 || strings.TrimSpace(string(out)) != `{"detail":"trigger not found"}` {
				t.Errorf("%s %.60s: %d %.120s", method, path, code, out)
			}
		}
	}
	if n := e.triggerCount(t, "ws-beta"); n != 1 {
		t.Fatalf("a foreign call changed ws-beta: %d rules", n)
	}
	e.replay(t, func(c autoCase) bool {
		return strings.HasSuffix(c.name, "_foreign_ws") || strings.HasSuffix(c.name, "_unknown")
	})
}

func TestAutoLimitsProfileFixtures(t *testing.T) {
	e := newAutoEnv(t, "limits")
	n := 0
	e.replay(t, func(c autoCase) bool { n++; return true })
	if n < 6 {
		t.Fatalf("only %d limits fixtures replayed", n)
	}
}

func (e autoEnv) triggerCount(t *testing.T, ws string) int {
	t.Helper()
	var n int
	if err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM triggers`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func autoDetailOf(t *testing.T, body []byte) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("not JSON: %.200s", body)
	}
	return m["detail"]
}

// D4: caps are validated (>= 0 and finite). Python stores -1.0, and a NaN/inf or int4-overflowing value reaches the
// database (500).
func TestAutoBadCapValues422(t *testing.T) {
	e := newAutoEnv(t, "default")
	base := `{"name":"cap","trigger_type":"on_row_added",`
	for _, c := range []struct{ name, body string }{
		{"negative_spend", base + `"max_spend_usd_per_day":-1}`},
		{"negative_actions", base + `"max_actions_per_day":-1}`},
		{"string_nan", base + `"max_spend_usd_per_day":"nan"}`},
		{"string_inf", base + `"max_spend_usd_per_day":"inf"}`},
		{"string_neg_inf", base + `"max_spend_usd_per_day":"-inf"}`},
		{"huge_float", base + `"max_spend_usd_per_day":1e999}`},
		{"actions_overflow_int4", base + `"max_actions_per_day":99999999999}`},
		{"actions_fraction", base + `"max_actions_per_day":1.5}`},
		{"spend_list", base + `"max_spend_usd_per_day":[]}`},
	} {
		t.Run("create_"+c.name, func(t *testing.T) {
			code, out, _ := e.send("alice", "ws-alpha", "POST", "/api/automations/triggers", c.body)
			if code != 422 {
				t.Fatalf("%d %.200s", code, out)
			}
			autoDetailOf(t, out)
		})
	}
	for _, body := range []string{`{"max_spend_usd_per_day":-0.01}`, `{"max_actions_per_day":-5}`, `{"max_spend_usd_per_day":"nan"}`,
		`{"max_actions_per_day":99999999999}`} {
		t.Run("patch_"+body, func(t *testing.T) {
			code, out, _ := e.send("alice", "ws-alpha", "PATCH", "/api/automations/triggers/trg-a-09", body)
			if code != 422 {
				t.Fatalf("%d %.200s", code, out)
			}
		})
	}
	if n := e.triggerCount(t, "ws-alpha"); n != 10 {
		t.Fatalf("a rejected cap stored a rule: %d", n)
	}
	var spend float64
	var acts int
	_ = tenancy.InWorkspace(context.Background(), e.pool, "ws-alpha", func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT max_spend_usd_per_day, max_actions_per_day FROM triggers WHERE id = 'trg-a-09'`).Scan(&spend, &acts)
	})
	if spend != 1.5 || acts != 5 {
		t.Fatalf("a rejected patch changed the caps: %v %v", spend, acts)
	}
	// zero is a valid cap (recorded 200)
	if code, out, _ := e.send("alice", "ws-alpha", "POST", "/api/automations/triggers", base+`"max_spend_usd_per_day":0,"max_actions_per_day":0}`); code != 200 {
		t.Fatalf("zero caps: %d %s", code, out)
	}
}

// D4: a non-object element of actions or a non-object trigger_config is a 422 (the first two are FastAPI shapes, the
// action config is a rule error), never a 500 and never stored.
func TestAutoNonObjectActionElement422(t *testing.T) {
	e := newAutoEnv(t, "default")
	for _, c := range []struct{ name, body string }{
		{"number", `"actions":[1]`}, {"null", `"actions":[null]`}, {"list", `"actions":[[]]`}, {"string", `"actions":["webhook"]`},
		{"mixed", `"actions":[{"type":"webhook","config":{"url":"https://hooks.example.com/x"}}, 5]`},
		{"actions_object", `"actions":{}`}, {"actions_null", `"actions":null`},
		{"config_string", `"actions":[{"type":"webhook","config":"oops"}]`},
		{"config_list", `"actions":[{"type":"re_enrich","config":["enr_email"]}]`},
		{"config_null", `"actions":[{"type":"webhook","config":null}]`},
		{"trigger_config_null", `"trigger_config":null`}, {"trigger_config_string", `"trigger_config":"x"`},
	} {
		t.Run("create_"+c.name, func(t *testing.T) {
			code, out, _ := e.send("alice", "ws-alpha", "POST", "/api/automations/triggers",
				`{"name":"x","trigger_type":"on_row_added",`+c.body+`}`)
			if code != 422 {
				t.Fatalf("%d %.200s", code, out)
			}
			autoDetailOf(t, out)
		})
		if strings.HasPrefix(c.body, `"actions"`) || strings.HasPrefix(c.body, `"trigger_config"`) {
			t.Run("patch_"+c.name, func(t *testing.T) {
				code, out, _ := e.send("alice", "ws-alpha", "PATCH", "/api/automations/triggers/trg-a-01", `{`+c.body+`}`)
				if c.name == "actions_null" || c.name == "trigger_config_null" {
					if code != 200 { // Optional fields: null means unchanged
						t.Fatalf("%d %.200s", code, out)
					}
					return
				}
				if code != 422 {
					t.Fatalf("%d %.200s", code, out)
				}
			})
		}
	}
	if n := e.triggerCount(t, "ws-alpha"); n != 10 {
		t.Fatalf("a rejected action stored a rule: %d", n)
	}
	// the exact FastAPI shape of an element error
	_, out, _ := e.send("alice", "ws-alpha", "POST", "/api/automations/triggers", `{"name":"x","trigger_type":"on_row_added","actions":[1]}`)
	want := `{"detail":[{"type":"dict_type","loc":["body","actions",0],"msg":"Input should be a valid dictionary","input":1}]}`
	var g, w any
	_ = json.Unmarshal(out, &g)
	_ = json.Unmarshal([]byte(want), &w)
	if fmt.Sprint(g) != fmt.Sprint(w) {
		t.Fatalf("element error shape: %.200s", out)
	}
	e.replay(t, func(c autoCase) bool {
		return c.name == "create_actions_not_objects" || c.name == "create_trigger_config_list"
	})
}

func TestAutoPatchNameTooLong422(t *testing.T) {
	e := newAutoEnv(t, "default")
	code, out, _ := e.send("alice", "ws-alpha", "PATCH", "/api/automations/triggers/trg-a-01", `{"name":"`+strings.Repeat("n", 201)+`"}`)
	if code != 422 {
		t.Fatalf("%d %.200s", code, out)
	}
	code, out, _ = e.send("alice", "ws-alpha", "PATCH", "/api/automations/triggers/trg-a-01", `{"name":"`+strings.Repeat("n", 200)+`"}`)
	if code != 200 {
		t.Fatalf("200-char name: %d %.200s", code, out)
	}
}

func TestAutoDeleteResponseAndPauseResumeShapes(t *testing.T) {
	e := newAutoEnv(t, "default")
	for _, c := range []struct {
		method, path, want string
	}{
		{"POST", "/api/automations/triggers/trg-a-01/pause", `{"enabled":false}`},
		{"POST", "/api/automations/triggers/trg-a-01/resume", `{"enabled":true,"next_run_at":null}`},
		{"DELETE", "/api/automations/triggers/trg-a-01", `{"deleted":true}`},
	} {
		code, out, _ := e.send("alice", "ws-alpha", c.method, c.path, "")
		if code != 200 || strings.TrimSpace(string(out)) != c.want {
			t.Errorf("%s %s: %d %s (want %s)", c.method, c.path, code, out, c.want)
		}
	}
	// a schedule rule resumes with its next aligned run
	code, out, _ := e.send("alice", "ws-alpha", "POST", "/api/automations/triggers/trg-a-04/resume", "")
	var r struct {
		Enabled   bool    `json:"enabled"`
		NextRunAt *string `json:"next_run_at"`
	}
	if code != 200 || json.Unmarshal(out, &r) != nil || !r.Enabled || r.NextRunAt == nil {
		t.Fatalf("schedule resume: %d %s", code, out)
	}
}

// A created schedule rule enqueues exactly one trigger_eval job keyed sched:<id>:<next run> (the first run at
// anchor + n * interval after now, anchor = creation time) and
// writes the next run and the mirror row (Python schedule_bootstrap_for_trigger).
func TestAutoScheduleCreateEnqueuesAlignedJob(t *testing.T) {
	e := newAutoEnv(t, "default")
	for _, interval := range []string{"hourly", "daily", "weekly"} {
		code, out, _ := e.send("alice", "ws-alpha", "POST", "/api/automations/triggers",
			`{"name":"sched `+interval+`","trigger_type":"on_schedule","trigger_config":{"interval":"`+interval+`"},`+
				`"actions":[{"type":"webhook","config":{"url":"https://hooks.example.com/new"}}]}`)
		var r struct {
			ID        string  `json:"id"`
			NextRunAt *string `json:"next_run_at"`
			Anchor    *string `json:"schedule_anchor"`
		}
		if code != 200 || json.Unmarshal(out, &r) != nil || r.NextRunAt == nil || r.Anchor == nil {
			t.Fatalf("%s create: %d %s", interval, code, out)
		}
		var typ, fireKey, status, exec string
		var runAt time.Time
		var njobs int
		var mirrorEnabled bool
		var mirrorNext time.Time
		err := tenancy.InWorkspace(context.Background(), e.pool, "ws-alpha", func(tx pgx.Tx) error {
			if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE fire_key LIKE $1`, "sched:"+r.ID+":%").Scan(&njobs); err != nil {
				return err
			}
			if err := tx.QueryRow(context.Background(), `SELECT type, fire_key, status, executor, next_run_at FROM jobs WHERE fire_key LIKE $1`,
				"sched:"+r.ID+":%").Scan(&typ, &fireKey, &status, &exec, &runAt); err != nil {
				return err
			}
			return tx.QueryRow(context.Background(), `SELECT enabled, next_run_at FROM scheduled_triggers WHERE trigger_id = $1`, r.ID).Scan(&mirrorEnabled, &mirrorNext)
		})
		if err != nil || njobs != 1 {
			t.Fatalf("%s: jobs=%d err=%v", interval, njobs, err)
		}
		if typ != "trigger_eval" || status != "pending" || exec != "python" {
			t.Errorf("%s job: type=%s status=%s executor=%s", interval, typ, status, exec)
		}
		next, err := time.Parse(time.RFC3339Nano, *r.NextRunAt)
		if err != nil {
			t.Fatal(err)
		}
		anchor, err := time.Parse(time.RFC3339Nano, *r.Anchor)
		if err != nil {
			t.Fatal(err)
		}
		step := map[string]time.Duration{"hourly": time.Hour, "daily": 24 * time.Hour, "weekly": 7 * 24 * time.Hour}[interval]
		if d := next.Sub(anchor); d <= 0 || d%step != 0 || d > step {
			t.Errorf("%s next run %s is not the first anchor-aligned run (anchor %s) after now", interval, *r.NextRunAt, *r.Anchor)
		}
		if !next.After(time.Now().Add(-time.Minute)) {
			t.Errorf("%s next run %s is in the past", interval, *r.NextRunAt)
		}
		if !strings.HasPrefix(fireKey, "sched:"+r.ID+":") || !mirrorEnabled || !mirrorNext.Equal(next) || !runAt.Equal(next) {
			t.Errorf("%s: fire_key %q mirror(enabled=%v next=%s) job next_run %s response next %s", interval, fireKey, mirrorEnabled, mirrorNext, runAt, next)
		}
	}
	// a disabled schedule rule enqueues nothing
	_, out, _ := e.send("alice", "ws-alpha", "POST", "/api/automations/triggers", `{"name":"off","trigger_type":"on_schedule","trigger_config":{"interval":"daily"},"enabled":false}`)
	var r struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(out, &r)
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE fire_key LIKE $1`, "sched:"+r.ID+":%").Scan(&n)
	if n != 0 {
		t.Fatalf("disabled schedule rule enqueued %d jobs", n)
	}
}

// created_by is the caller's user id as text, and a webhook header secret is never echoed beyond its reference name.
func TestAutoCreateStampsCreatorAndNeverEchoesSecret(t *testing.T) {
	e := newAutoEnv(t, "default")
	_, out, _ := e.send("alice", "ws-alpha", "POST", "/api/automations/triggers",
		`{"name":"s","trigger_type":"on_row_added","actions":[{"type":"webhook","config":{"url":"https://hooks.example.com/x","header_name":"X-T","header_secret_ref":"`+autoSecretName+`"}}]}`)
	var r struct {
		CreatedBy string `json:"created_by"`
	}
	if json.Unmarshal(out, &r) != nil || r.CreatedBy != "1" {
		t.Fatalf("created_by: %s", out)
	}
	if strings.Contains(string(out), "test-only-not-a-secret") {
		t.Fatal("response echoes the secret value")
	}
	// workspace-only secrets: ws-beta cannot reference ws-alpha's secret
	code, out2, _ := e.send("bob", "ws-beta", "POST", "/api/automations/triggers",
		`{"name":"s","trigger_type":"on_row_added","actions":[{"type":"webhook","config":{"url":"https://hooks.example.com/x","header_secret_ref":"`+autoSecretName+`"}}]}`)
	if code != 422 || strings.Contains(string(out2), "test-only-not-a-secret") {
		t.Fatalf("foreign secret ref: %d %s", code, out2)
	}
}
