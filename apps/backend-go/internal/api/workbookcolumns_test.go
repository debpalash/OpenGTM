//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type colEnv struct{ mcpEnv }

func newColEnv(t *testing.T) colEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	RegisterWorkbookColumns(r, Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Flags: config.Flags{PGLeadStore: true}})
	e := colEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}
	e.purge()
	t.Cleanup(e.purge)
	return e
}

func (e colEnv) purge() {
	ctx := context.Background()
	for _, ws := range []string{"ws-alpha", "ws-beta", "t11-ws"} {
		_ = tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			for _, tbl := range []string{"workbook_enrichments", "workbook_views", "workbook_rows", "workbooks"} {
				if _, err := tx.Exec(ctx, "DELETE FROM "+tbl); err != nil {
					return err
				}
			}
			return nil
		})
	}
}

// seedSeed mirrors legacy/python/scripts/contract_seed.py (users, memberships, workbooks, rows, views).
func (e colEnv) seedSeed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, u := range []struct {
		name  string
		admin bool
	}{{"alice", true}, {"bob", false}, {"erin", false}} {
		role := "user"
		if u.admin {
			role = "admin"
		}
		e.seed(t, u.name, pythonHash, true, role, u.admin)
	}
	alice, bob, erin := e.mustUserByName(t, "alice"), e.mustUserByName(t, "bob"), e.mustUserByName(t, "erin")
	e.workspace(t, "ws-alpha", alice.ID)
	e.workspace(t, "ws-beta", bob.ID)
	e.member(t, "ws-alpha", alice.ID, "owner")
	e.member(t, "ws-alpha", bob.ID, "member")
	e.member(t, "ws-alpha", erin.ID, "editor")
	e.member(t, "ws-beta", bob.ID, "owner")
	alpha2 := `[{"id":"company","name":"Company","type":"lead_field","width":200,"lead_field":"company"},
{"id":"website","name":"Website","type":"lead_field","width":200,"lead_field":"website"},
{"id":"email","name":"Email","type":"lead_field","width":220,"lead_field":"email"},
{"id":"city","name":"City","type":"lead_field","width":150,"lead_field":"city"},
{"id":"phone","name":"Phone","type":"lead_field","width":150,"lead_field":"phone"},
{"id":"email_wf","name":"Email Finder","type":"waterfall","width":220,"waterfall":["hunter","apollo"],"target_field":"email","verify":true},
{"id":"summary","name":"Summary","type":"ai_formula","width":300,"prompt":"Summarize {Company} in one sentence","input_columns":["company"]}]`
	for _, w := range []struct {
		id, ws, name, desc, cols string
		total                    int
	}{
		{"wb-alpha-1", "ws-alpha", "Alpha Empty", "empty workbook", "[]", 0},
		{"wb-alpha-2", "ws-alpha", "Alpha Leads", "25 seeded rows", alpha2, 25},
		{"wb-beta-1", "ws-beta", "Beta Workbook", "beta tenant", "[]", 0},
	} {
		err := tenancy.InWorkspace(ctx, e.pool, w.ws, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO workbooks (id,name,description,status,workspace_id,source_type,source_config,filter_criteria,columns_config,total_rows,completed_rows,sync_to_leads,budget_spent_usd,created_at,updated_at)
			 VALUES ($1,$2,$3,'idle',$4,'manual','{}'::json,'{}'::json,$5::json,$6,0,false,0,'2026-08-29 10:00:00','2026-08-29 10:00:00')`, w.id, w.name, w.desc, w.ws, w.cols, w.total)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	err := tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
		for i := 1; i <= 25; i++ {
			if _, err := tx.Exec(ctx, `INSERT INTO workbook_rows (workbook_id,position,data,enrichments,created_at,updated_at,workspace_id)
			 VALUES ('wb-alpha-2',$1,$2::json,'{"email_wf":{"value":"x"},"summary":{"value":"y"}}'::json,'2026-08-29 10:00:00','2026-08-29 10:00:00','ws-alpha')`, i-1, fmt.Sprintf(`{"company":"Acme %02d"}`, i)); err != nil {
				return err
			}
		}
		for _, v := range [][3]string{
			{"view-alpha-1", "Berlin only", `{"filters": [{"column": "city", "op": "equals", "value": "Berlin"}], "hidden_columns": []}`},
			{"view-alpha-2", "Sorted by company", `{"sort": [{"column": "company", "dir": "asc"}], "hidden_columns": ["phone"]}`},
		} {
			if _, err := tx.Exec(ctx, `INSERT INTO workbook_views (id,workspace_id,workbook_id,name,config,created_at,updated_at)
			 VALUES ($1,'ws-alpha','wb-alpha-2',$2,$3::json,'2026-08-29 10:00:00','2026-08-29 10:00:00')`, v[0], v[1], v[2]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e colEnv) hdr(who, ws string) map[string]string {
	if who == "" {
		return nil
	}
	h := bearer(who)
	h["X-Workspace-Id"] = ws
	return h
}

func (e colEnv) call(who, ws, method, path, body string) (int, string) {
	rec, _ := e.do(method, path, "application/json", body, e.hdr(who, ws))
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

// ownWS creates a private workspace with one editor ("t11-ed") for tests that need their own workbooks.
func (e colEnv) ownWS(t *testing.T) {
	t.Helper()
	e.seed(t, "t11-ed", pythonHash, true, "user", false)
	u := e.mustUserByName(t, "t11-ed")
	e.workspace(t, "t11-ws", u.ID)
	e.member(t, "t11-ws", u.ID, "editor")
}

func (e colEnv) ed(method, path, body string) (int, string) {
	return e.call("t11-ed", "t11-ws", method, path, body)
}

func (e colEnv) mkWB(t *testing.T, ws, id, cols string) {
	t.Helper()
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workbooks (id,name,workspace_id,columns_config,created_at,updated_at) VALUES ($1,$1,$2,$3::json,'2026-01-01 00:00:00','2026-01-01 00:00:00')`, id, ws, cols)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e colEnv) cols(t *testing.T, ws, id string) (string, time.Time) {
	t.Helper()
	ctx := context.Background()
	var c string
	var u time.Time
	err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(columns_config::text,'null'), updated_at FROM workbooks WHERE id=$1`, id).Scan(&c, &u)
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, u
}

func (e colEnv) exec(t *testing.T, ws, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	if err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, sql, args...); return err }); err != nil {
		t.Fatal(err)
	}
}

func generic(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("%v: %s", err, s)
	}
	return v
}

// columnReplay is the recorded order of legacy/python/scripts/contract_cases/columns.yaml
// (state carries across mutating cases) with the actor each case ran as.
var columnReplay = []struct{ name, who, ws string }{
	{"settings_no_token", "", ""},
	{"settings_viewer", "bob", "ws-alpha"},
	{"settings_other_workspace", "bob", "ws-beta"},
	{"settings_empty_changes", "alice", "ws-alpha"},
	{"settings_keys_mismatch", "alice", "ws-alpha"},
	{"settings_unsupported_id", "alice", "ws-alpha"},
	{"settings_unsupported_type", "alice", "ws-alpha"},
	{"settings_unsupported_width", "alice", "ws-alpha"},
	{"settings_unsupported_foo", "alice", "ws-alpha"},
	{"settings_extra_field", "alice", "ws-alpha"},
	{"settings_missing_expected", "alice", "ws-alpha"},
	{"settings_bad_types", "alice", "ws-alpha"},
	{"settings_body_list", "alice", "ws-alpha"},
	{"settings_no_body", "alice", "ws-alpha"},
	{"settings_wb_missing", "alice", "ws-alpha"},
	{"settings_col_missing", "alice", "ws-alpha"},
	{"settings_stale_expected", "alice", "ws-alpha"},
	{"settings_invalid_int_string", "alice", "ws-alpha"},
	{"settings_invalid_bool_int", "alice", "ws-alpha"},
	{"settings_invalid_budget_negative", "alice", "ws-alpha"},
	{"settings_invalid_budget_string", "alice", "ws-alpha"},
	{"settings_invalid_policy", "alice", "ws-alpha"},
	{"settings_invalid_list", "alice", "ws-alpha"},
	{"settings_name_blank", "alice", "ws-alpha"},
	{"settings_name_null", "alice", "ws-alpha"},
	{"settings_ok_verify", "alice", "ws-alpha"},
	{"settings_ok_noop", "alice", "ws-alpha"},
	{"settings_ok_budget", "alice", "ws-alpha"},
	{"settings_ok_policy", "alice", "ws-alpha"},
	{"settings_ok_dict_order", "alice", "ws-alpha"},
	{"settings_ok_rename_free", "alice", "ws-alpha"},
	{"settings_ok_prompt_html", "alice", "ws-alpha"},
	{"settings_rename_blocked", "alice", "ws-alpha"},
	{"settings_cycle_self", "alice", "ws-alpha"},
	{"settings_ok_dep", "alice", "ws-alpha"},
	{"settings_cycle_two", "alice", "ws-alpha"},
	{"settings_ok_null", "alice", "ws-alpha"},
	{"settings_ok_by_erin", "erin", "ws-alpha"},
	{"get_after_settings", "alice", "ws-alpha"},
	{"order_no_token", "", ""},
	{"order_viewer", "bob", "ws-alpha"},
	{"order_missing_fields", "alice", "ws-alpha"},
	{"order_bad_types", "alice", "ws-alpha"},
	{"order_extra", "alice", "ws-alpha"},
	{"order_body_list", "alice", "ws-alpha"},
	{"order_wb_missing", "alice", "ws-alpha"},
	{"order_stale", "alice", "ws-alpha"},
	{"order_too_short", "alice", "ws-alpha"},
	{"order_duplicate", "alice", "ws-alpha"},
	{"order_unknown_id", "alice", "ws-alpha"},
	{"order_ok_reverse", "alice", "ws-alpha"},
	{"order_ok_noop", "alice", "ws-alpha"},
	{"order_ok_restore", "alice", "ws-alpha"},
	{"get_after_order", "alice", "ws-alpha"},
	{"width_no_token", "", ""},
	{"width_viewer", "bob", "ws-alpha"},
	{"width_low", "alice", "ws-alpha"},
	{"width_high", "alice", "ws-alpha"},
	{"width_float", "alice", "ws-alpha"},
	{"width_float_frac", "alice", "ws-alpha"},
	{"width_string", "alice", "ws-alpha"},
	{"width_bool", "alice", "ws-alpha"},
	{"width_missing", "alice", "ws-alpha"},
	{"width_null", "alice", "ws-alpha"},
	{"width_body_list", "alice", "ws-alpha"},
	{"width_ok_extra_ignored", "alice", "ws-alpha"},
	{"width_ok", "alice", "ws-alpha"},
	{"width_ok_max", "alice", "ws-alpha"},
	{"width_noop", "alice", "ws-alpha"},
	{"width_unknown_col", "alice", "ws-alpha"},
	{"width_wb_missing", "alice", "ws-alpha"},
	{"get_after_width", "alice", "ws-alpha"},
	{"add_no_token", "", ""},
	{"add_viewer", "bob", "ws-alpha"},
	{"add_missing_column", "alice", "ws-alpha"},
	{"add_column_null", "alice", "ws-alpha"},
	{"add_body_list", "alice", "ws-alpha"},
	{"add_bad_types", "alice", "ws-alpha"},
	{"add_blank_id", "alice", "ws-alpha"},
	{"add_blank_name", "alice", "ws-alpha"},
	{"add_width_low", "alice", "ws-alpha"},
	{"add_width_high", "alice", "ws-alpha"},
	{"add_dup_id", "alice", "ws-alpha"},
	{"add_cycle_self", "alice", "ws-alpha"},
	{"add_cycle_by_name", "alice", "ws-alpha"},
	{"add_wb_missing", "alice", "ws-alpha"},
	{"add_other_workspace", "bob", "ws-beta"},
	{"add_ok_defaults", "alice", "ws-alpha"},
	{"add_ok_full", "alice", "ws-alpha"},
	{"add_ok_lax_coercion", "alice", "ws-alpha"},
	{"add_ok_by_erin", "erin", "ws-alpha"},
	{"add_to_empty", "alice", "ws-alpha"},
	{"get_after_add", "alice", "ws-alpha"},
	{"delete_no_token", "", ""},
	{"delete_viewer", "bob", "ws-alpha"},
	{"delete_wb_missing", "alice", "ws-alpha"},
	{"delete_col_missing", "alice", "ws-alpha"},
	{"delete_other_workspace", "bob", "ws-beta"},
	{"delete_bad_body", "alice", "ws-alpha"},
	{"delete_body_list", "alice", "ws-alpha"},
	{"delete_extra_field", "alice", "ws-alpha"},
	{"delete_stale_expected", "alice", "ws-alpha"},
	{"delete_referenced_by_column", "alice", "ws-alpha"},
	{"delete_ok_null_body", "alice", "ws-alpha"},
	{"delete_referenced_by_view_filter", "alice", "ws-alpha"},
	{"delete_referenced_by_view_hidden", "alice", "ws-alpha"},
	{"delete_referenced_by_view_sort", "alice", "ws-alpha"},
	{"delete_ok_with_expected", "alice", "ws-alpha"},
	{"delete_ok_no_body", "alice", "ws-alpha"},
	{"delete_ok_by_erin", "alice", "ws-alpha"},
	{"delete_from_empty", "alice", "ws-alpha"},
	{"get_after_delete", "alice", "ws-alpha"},
}

func TestColumnsMatchFixtures(t *testing.T) {
	e := newColEnv(t)
	e.seedSeed(t)
	seen := map[string]bool{}
	for _, c := range columnReplay {
		fx := contract.Load(t, "columns/"+c.name)
		seen["columns/"+c.name] = true
		if strings.HasPrefix(c.name, "get_after_") {
			// The recorded state of every workbook (GET /api/workbooks/) against the Go database.
			var want struct {
				Workbooks []struct {
					ID      string          `json:"id"`
					Columns json.RawMessage `json:"columns_config"`
				} `json:"workbooks"`
			}
			if err := json.Unmarshal(fx.Response.Body, &want); err != nil {
				t.Fatal(err)
			}
			for _, w := range want.Workbooks {
				ws := "ws-alpha"
				if strings.HasPrefix(w.ID, "wb-beta") {
					ws = "ws-beta"
				}
				got, _ := e.cols(t, ws, w.ID)
				if !reflect.DeepEqual(generic(t, got), generic(t, string(w.Columns))) {
					t.Errorf("%s: %s columns_config\n got %s\nwant %s", c.name, w.ID, got, w.Columns)
				}
			}
			continue
		}
		hdr := map[string]string{}
		if c.who != "" {
			hdr = e.hdr(c.who, c.ws)
		}
		_, got := e.do(fx.Request.Method, fx.Request.Path, "application/json", fixtureBody(fx), hdr)
		t.Run(c.name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response, contract.IgnorePaths("$.updated_at")) })
	}
	for _, n := range contractFixtureNames(t, "columns") {
		if !seen[n] {
			t.Errorf("fixture never replayed: %s", n)
		}
	}
}

// Python's stored-key order is observable: assert it explicitly (fixtures are key-sorted).
func TestResponseKeyOrderMatchesPython(t *testing.T) {
	e := newColEnv(t)
	e.ownWS(t)
	e.mkWB(t, "t11-ws", "t11-order", `[{"id":"a","name":"A","type":"lead_field","width":200,"zz":1}]`)
	code, body := e.ed("PATCH", "/api/workbooks/t11-order/columns/a/settings", `{"changes":{"verify":true,"formula":"x","name":"B","icp":{"z":1,"a":2}},"expected":{"verify":null,"formula":null,"name":"A","icp":null}}`)
	if code != 200 || body != `{"column_id":"a","changes":{"verify":true,"formula":"x","name":"B","icp":{"z":1,"a":2}}}` {
		t.Fatalf("%d %s", code, body)
	}
	got, _ := e.cols(t, "t11-ws", "t11-order")
	if compactNoSpace(got) != `[{"id":"a","name":"B","type":"lead_field","width":200,"zz":1,"verify":true,"formula":"x","icp":{"z":1,"a":2}}]` {
		t.Fatalf("stored: %s", got)
	}
	code, body = e.ed("POST", "/api/workbooks/t11-order/columns", `{"column":{"width":150,"name":"N","id":"n","max_tokens":9,"zzz":3,"formula":"<b>&"}}`)
	if code != 200 || !strings.Contains(body, `{"id":"n","name":"N","type":"lead_field","width":150,"max_tokens":9,"formula":"<b>&"}]`) {
		t.Fatalf("%d %s", code, body)
	}
	if !strings.HasPrefix(body, `{"id":"t11-order","name":"t11-order","description":"","status":"draft","source_type":`) {
		t.Fatalf("workbook key order: %s", body)
	}
}

func compactNoSpace(s string) string {
	return strings.NewReplacer(", ", ",", ": ", ":").Replace(s)
}

func TestOrderLiteralNotCapturedByColumnID(t *testing.T) {
	e := newColEnv(t)
	e.ownWS(t)
	e.mkWB(t, "t11-ws", "t11-lit", `[{"id":"order","name":"Order","type":"lead_field","width":200},{"id":"b","name":"B","type":"lead_field","width":200}]`)
	code, body := e.ed("PATCH", "/api/workbooks/t11-lit/columns/order", `{"column_ids":["b","order"],"expected_column_ids":["order","b"]}`)
	if code != 200 || body != `{"column_ids":["b","order"]}` {
		t.Fatalf("columns/order literal: %d %s", code, body)
	}
	// A column literally named "order" still has its own width/settings/delete routes.
	if code, body = e.ed("PATCH", "/api/workbooks/t11-lit/columns/order/width", `{"width":300}`); code != 200 || body != `{"column_id":"order","width":300}` {
		t.Fatalf("width: %d %s", code, body)
	}
	if code, body = e.ed("PATCH", "/api/workbooks/t11-lit/columns/order/settings", `{"changes":{"verify":true},"expected":{"verify":null}}`); code != 200 {
		t.Fatalf("settings: %d %s", code, body)
	}
	if code, _ = e.ed("DELETE", "/api/workbooks/t11-lit/columns/order", ""); code != 200 {
		t.Fatalf("delete: %d", code)
	}
}

func TestGuardStrings(t *testing.T) {
	e := newColEnv(t)
	e.ownWS(t)
	two := `[{"id":"a","name":"A","type":"lead_field","width":200},{"id":"a","name":"A2","type":"lead_field","width":200}]`
	e.mkWB(t, "t11-ws", "t11-dup", two)
	e.mkWB(t, "t11-ws", "t11-g", `[{"id":"a","name":"A","type":"lead_field","width":200},{"id":"b","name":"B","type":"lead_field","width":200}]`)
	for _, c := range []struct {
		name, method, path, body string
		status                   int
		want                     string
	}{
		{"settings ambiguous", "PATCH", "/api/workbooks/t11-dup/columns/a/settings", `{"changes":{"verify":true},"expected":{"verify":null}}`, 409, "Column identity is ambiguous"},
		{"delete ambiguous", "DELETE", "/api/workbooks/t11-dup/columns/a", "", 409, "Column identity is ambiguous"},
		{"settings unsupported", "PATCH", "/api/workbooks/t11-g/columns/a/settings", `{"changes":{"width":100},"expected":{"width":200}}`, 422, "Provide supported changes and matching expected fields"},
		{"settings col missing", "PATCH", "/api/workbooks/t11-g/columns/zz/settings", `{"changes":{"verify":true},"expected":{"verify":null}}`, 404, "Column not found"},
		{"settings stale", "PATCH", "/api/workbooks/t11-g/columns/a/settings", `{"changes":{"verify":true},"expected":{"verify":false}}`, 409, "Column settings changed. Refresh before saving again."},
		{"settings invalid", "PATCH", "/api/workbooks/t11-g/columns/a/settings", `{"changes":{"max_steps":"3"},"expected":{"max_steps":null}}`, 422, "Invalid column settings"},
		{"settings blank name", "PATCH", "/api/workbooks/t11-g/columns/a/settings", `{"changes":{"name":" \u00a0"},"expected":{"name":"A"}}`, 422, "Column name must not be empty"},
		{"order stale", "PATCH", "/api/workbooks/t11-g/columns/order", `{"column_ids":["b","a"],"expected_column_ids":["b","a"]}`, 409, "Column order changed. Refresh before reordering."},
		{"order ambiguous current", "PATCH", "/api/workbooks/t11-dup/columns/order", `{"column_ids":["a","a"],"expected_column_ids":["a","a"]}`, 422, "Column order must contain each current column exactly once"},
		{"order missing id", "PATCH", "/api/workbooks/t11-g/columns/order", `{"column_ids":["a"],"expected_column_ids":["a","b"]}`, 422, "Column order must contain each current column exactly once"},
		{"order wrong id", "PATCH", "/api/workbooks/t11-g/columns/order", `{"column_ids":["a","x"],"expected_column_ids":["a","b"]}`, 422, "Column order must contain each current column exactly once"},
		{"width col missing", "PATCH", "/api/workbooks/t11-g/columns/zz/width", `{"width":100}`, 404, "Column not found"},
		{"add blank id", "POST", "/api/workbooks/t11-g/columns", `{"column":{"id":" ","name":"N"}}`, 422, "Column requires a name, identity and width from 80 to 600"},
		{"add blank name", "POST", "/api/workbooks/t11-g/columns", `{"column":{"id":"n","name":"\t"}}`, 422, "Column requires a name, identity and width from 80 to 600"},
		{"add width 79", "POST", "/api/workbooks/t11-g/columns", `{"column":{"id":"n","name":"N","width":79}}`, 422, "Column requires a name, identity and width from 80 to 600"},
		{"add width 601", "POST", "/api/workbooks/t11-g/columns", `{"column":{"id":"n","name":"N","width":601}}`, 422, "Column requires a name, identity and width from 80 to 600"},
		{"add duplicate", "POST", "/api/workbooks/t11-g/columns", `{"column":{"id":"a","name":"N"}}`, 409, "Column identity already exists. Refresh before adding another column."},
		{"delete col missing", "DELETE", "/api/workbooks/t11-g/columns/zz", "", 404, "Column not found"},
		{"delete stale", "DELETE", "/api/workbooks/t11-g/columns/a", `{"expected_column":{"id":"a","name":"A","type":"lead_field","width":201}}`, 409, "Column changed since confirmation. Refresh and review before deleting."},
		{"workbook missing", "DELETE", "/api/workbooks/nope/columns/a", "", 404, "Workbook not found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, body := e.ed(c.method, c.path, c.body)
			if code != c.status || body != `{"detail":`+jsonString(c.want)+`}` {
				t.Fatalf("%d %s", code, body)
			}
		})
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestSettingsPatchRenameBlockedByReference(t *testing.T) {
	e := newColEnv(t)
	e.ownWS(t)
	want := `{"detail":"Rename breaks column-name references in: Summary. Change those references to stable column IDs first."}`
	for name, dep := range map[string]string{
		"prompt":        `"type":"ai_formula","prompt":"x {Email Finder} y"`,
		"input display": `"type":"ai_formula","input_columns":["email finder"]`,
		"http_url":      `"type":"http","http_url":"https://x/{ Email_Finder }"`,
		"http header":   `"type":"http","http_headers":{"a":{"b":["{Email Finder}"]}}`,
		"condition":     `"type":"conditional","condition":"{Email Finder} == 1"`,
		"goal":          `"type":"agent","goal":"find {email finder}"`,
	} {
		t.Run(name, func(t *testing.T) {
			id := "t11-rn-" + strings.ReplaceAll(name, " ", "-")
			e.mkWB(t, "t11-ws", id, `[{"id":"email_wf","name":"Email Finder","type":"waterfall","width":220},{"id":"summary","name":"Summary","width":300,`+dep+`}]`)
			before, _ := e.cols(t, "t11-ws", id)
			code, body := e.ed("PATCH", "/api/workbooks/"+id+"/columns/email_wf/settings", `{"changes":{"name":"Email Lookup"},"expected":{"name":"Email Finder"}}`)
			if code != 409 || body != want {
				t.Fatalf("%d %s", code, body)
			}
			if after, _ := e.cols(t, "t11-ws", id); after != before {
				t.Fatal("blocked rename wrote")
			}
		})
	}
	// A stable-id reference survives the rename.
	e.mkWB(t, "t11-ws", "t11-rn-ok", `[{"id":"email_wf","name":"Email Finder","type":"waterfall","width":220},{"id":"summary","name":"Summary","type":"ai_formula","width":300,"input_columns":["email_wf"]}]`)
	if code, body := e.ed("PATCH", "/api/workbooks/t11-rn-ok/columns/email_wf/settings", `{"changes":{"name":"Email Lookup"},"expected":{"name":"Email Finder"}}`); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
}

func TestSettingsPatchCycleBlocked(t *testing.T) {
	e := newColEnv(t)
	e.ownWS(t)
	e.mkWB(t, "t11-ws", "t11-cy", `[{"id":"a","name":"Alpha","type":"ai_formula","width":200,"input_columns":["b"]},{"id":"b","name":"Beta","type":"ai_formula","width":200,"prompt":"hi"},{"id":"c","name":"Gamma","type":"formula","width":200,"formula":"1"}]`)
	for _, c := range []struct{ name, path, body, want string }{
		{"self", "b", `{"changes":{"input_columns":["b"]},"expected":{"input_columns":null}}`, "b"},
		{"two cycle by name", "b", `{"changes":{"prompt":"{Alpha}"},"expected":{"prompt":"hi"}}`, "a, b"},
		{"chain without a loop is allowed", "b", `{"changes":{"input_columns":["c"]},"expected":{"input_columns":null}}`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			before, _ := e.cols(t, "t11-ws", "t11-cy")
			code, body := e.ed("PATCH", "/api/workbooks/t11-cy/columns/"+c.path+"/settings", c.body)
			if c.want == "" {
				if code != 200 { // b -> c is not a cycle (c has no refs)
					t.Fatalf("%d %s", code, body)
				}
				return
			}
			w := `{"detail":"Column changes introduce circular dependencies affecting: ` + c.want + `. Break the circular references before saving."}`
			if code != 422 || body != w {
				t.Fatalf("%d %s", code, body)
			}
			if after, _ := e.cols(t, "t11-ws", "t11-cy"); after != before {
				t.Fatal("blocked cycle wrote")
			}
		})
	}
	// Closing a loop through c (b -> c -> a -> b) names every member, ids sorted.
	code, body := e.ed("PATCH", "/api/workbooks/t11-cy/columns/c/settings", `{"changes":{"formula":"{Alpha}"},"expected":{"formula":"1"}}`)
	if code != 422 || body != `{"detail":"Column changes introduce circular dependencies affecting: a, b, c. Break the circular references before saving."}` {
		t.Fatalf("%d %s", code, body)
	}
}

func TestWidthBounds(t *testing.T) {
	e := newColEnv(t)
	e.ownWS(t)
	e.mkWB(t, "t11-ws", "t11-w", `[{"id":"a","name":"A","width":200}]`)
	for _, c := range []struct {
		body   string
		status int
	}{{`{"width":79}`, 422}, {`{"width":80}`, 200}, {`{"width":600}`, 200}, {`{"width":601}`, 422}, {`{"width":300.0}`, 422}, {`{"width":"300"}`, 422}, {`{"width":true}`, 422}, {`{}`, 422}} {
		if code, body := e.ed("PATCH", "/api/workbooks/t11-w/columns/a/width", c.body); code != c.status {
			t.Errorf("%s: %d %s", c.body, code, body)
		}
	}
	got, _ := e.cols(t, "t11-ws", "t11-w")
	if compactNoSpace(got) != `[{"id":"a","name":"A","width":600}]` {
		t.Fatalf("%s", got)
	}
}

func TestAddColumnCycle422(t *testing.T) {
	e := newColEnv(t)
	e.ownWS(t)
	e.mkWB(t, "t11-ws", "t11-ac", `[{"id":"a","name":"Alpha","type":"ai_formula","width":200,"input_columns":["x"]}]`)
	before, _ := e.cols(t, "t11-ws", "t11-ac")
	code, body := e.ed("POST", "/api/workbooks/t11-ac/columns", `{"column":{"id":"x","name":"X","type":"formula","formula":"{Alpha}"}}`)
	if code != 422 || body != `{"detail":"Column changes introduce circular dependencies affecting: a, x. Break the circular references before saving."}` {
		t.Fatalf("%d %s", code, body)
	}
	if after, _ := e.cols(t, "t11-ws", "t11-ac"); after != before {
		t.Fatal("blocked add wrote")
	}
	// A pre-existing cycle is not new: adding an unrelated column still works.
	e.mkWB(t, "t11-ws", "t11-ac2", `[{"id":"a","name":"A","type":"ai_formula","width":200,"input_columns":["a"]}]`)
	if code, body = e.ed("POST", "/api/workbooks/t11-ac2/columns", `{"column":{"id":"n","name":"N"}}`); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
}

func TestDeleteBlockedByView(t *testing.T) {
	e := newColEnv(t)
	e.ownWS(t)
	e.mkWB(t, "t11-ws", "t11-dv", `[{"id":"city","name":"City","width":200},{"id":"phone","name":"Phone","width":200},{"id":"co","name":"Co","width":200},{"id":"free","name":"Free","width":200}]`)
	for i, v := range [][2]string{
		{"Z view", `{"filters":[{"column":"city","op":"equals","value":"x"}]}`},
		{"A view", `{"sort":[{"column":"city","dir":"asc"}],"hidden_columns":["phone"]}`},
		{"Bad view", `{"filters":null,"sort":"x","hidden_columns":[1,["city"],null]}`},
		{"Scalar view", `5`},
		{"Null view", `null`},
	} {
		e.exec(t, "t11-ws", `INSERT INTO workbook_views (id,workspace_id,workbook_id,name,config,created_at,updated_at) VALUES ($1,'t11-ws','t11-dv',$2,$3::json,$4,$4)`, fmt.Sprintf("t11-v%d", i), v[0], v[1], time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC))
	}
	code, body := e.ed("DELETE", "/api/workbooks/t11-dv/columns/city", "")
	if code != 409 || body != `{"detail":"Column is referenced by saved views: Z view, A view. Update those views before deleting."}` {
		t.Fatalf("%d %s", code, body)
	}
	code, body = e.ed("DELETE", "/api/workbooks/t11-dv/columns/phone", "")
	if code != 409 || body != `{"detail":"Column is referenced by saved views: A view. Update those views before deleting."}` {
		t.Fatalf("%d %s", code, body)
	}
	// Malformed saved-view configs are ignored (Python raised a 500 on them: documented hardening).
	if code, body = e.ed("DELETE", "/api/workbooks/t11-dv/columns/co", ""); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if code, _ = e.ed("DELETE", "/api/workbooks/t11-dv/columns/free", ""); code != 200 {
		t.Fatal(code)
	}
}

func TestDeleteBlockedByColumnReference(t *testing.T) {
	e := newColEnv(t)
	e.ownWS(t)
	e.mkWB(t, "t11-ws", "t11-dc", `[{"id":"a","name":"Alpha","width":200},{"id":"s1","name":"S1","type":"ai_formula","input_columns":["a"]},{"id":"s2","name":"","type":"ai_formula","prompt":"{alpha}"}]`)
	code, body := e.ed("DELETE", "/api/workbooks/t11-dc/columns/a", "")
	if code != 409 || body != `{"detail":"Column is referenced by: S1, s2. Update those references before deleting."}` {
		t.Fatalf("%d %s", code, body)
	}
}

func TestDeleteRemovesColumnValuesFromRowsOrLeavesThemAsPython(t *testing.T) {
	// Python (workbooks.py remove_column) deletes only workbook_enrichments rows of the column;
	// workbook_rows.data / .enrichments JSON is left untouched.
	e := newColEnv(t)
	e.ownWS(t)
	e.mkWB(t, "t11-ws", "t11-de", `[{"id":"a","name":"A","width":200},{"id":"b","name":"B","width":200}]`)
	e.mkWB(t, "t11-ws", "t11-de2", `[{"id":"a","name":"A","width":200}]`)
	e.exec(t, "t11-ws", `INSERT INTO workbook_rows (workbook_id,position,data,enrichments,workspace_id) VALUES ('t11-de',0,'{"a":"v"}'::json,'{"a":{"value":"x"},"b":{"value":"y"}}'::json,'t11-ws')`)
	for _, wb := range []string{"t11-de", "t11-de2"} {
		for _, c := range []string{"a", "b"} {
			e.exec(t, "t11-ws", `INSERT INTO workbook_enrichments (workbook_id,lead_id,column_id,value,workspace_id) VALUES ($1,1,$2,'v','t11-ws')`, wb, c)
		}
	}
	if code, body := e.ed("DELETE", "/api/workbooks/t11-de/columns/a", ""); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	count := func(where string) (n int) {
		ctx := context.Background()
		_ = tenancy.InWorkspace(ctx, e.pool, "t11-ws", func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT count(*) FROM "+where).Scan(&n)
		})
		return
	}
	if n := count(`workbook_enrichments WHERE workbook_id='t11-de' AND column_id='a'`); n != 0 {
		t.Fatalf("enrichments of the column left: %d", n)
	}
	if n := count(`workbook_enrichments WHERE workbook_id='t11-de' AND column_id='b'`); n != 1 {
		t.Fatalf("other column's enrichments: %d", n)
	}
	if n := count(`workbook_enrichments WHERE workbook_id='t11-de2'`); n != 2 {
		t.Fatalf("other workbook's enrichments touched: %d", n)
	}
	if n := count(`workbook_rows WHERE workbook_id='t11-de' AND enrichments::text LIKE '%"a"%' AND data::text LIKE '%"a"%'`); n != 1 {
		t.Fatalf("row JSON must be untouched (as Python): %d", n)
	}
}

func TestConcurrentColumnEditsSerialize(t *testing.T) {
	e := newColEnv(t)
	e.ownWS(t)
	e.mkWB(t, "t11-ws", "t11-cc", `[{"id":"a","name":"A","width":200}]`)
	var wg sync.WaitGroup
	res := make([]string, 6)
	codes := make([]int, 6)
	for i := range res {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], res[i] = e.ed("PATCH", "/api/workbooks/t11-cc/columns/a/settings", fmt.Sprintf(`{"changes":{"formula":"f%d"},"expected":{"formula":null}}`, i))
		}(i)
	}
	wg.Wait()
	ok := 0
	for i := range res {
		switch codes[i] {
		case 200:
			ok++
		case 409:
			if res[i] != `{"detail":"Column settings changed. Refresh before saving again."}` {
				t.Errorf("409 body: %s", res[i])
			}
		default:
			t.Errorf("unexpected %d %s", codes[i], res[i])
		}
	}
	if ok != 1 {
		t.Fatalf("exactly one edit must win, got %d", ok)
	}
}

func TestMutationsAdvanceUpdatedAtOnlyWhenChanged(t *testing.T) {
	e := newColEnv(t)
	e.ownWS(t)
	e.mkWB(t, "t11-ws", "t11-ua", `[{"id":"a","name":"A","type":"lead_field","width":200},{"id":"b","name":"B","type":"lead_field","width":200}]`)
	e.mkWB(t, "t11-ws", "t11-ub", `[{"id":"a","name":"A","type":"lead_field","width":200}]`)
	for _, c := range []struct {
		name, method, path, body string
		advances                 bool
	}{
		{"settings changes", "PATCH", "/api/workbooks/t11-ua/columns/a/settings", `{"changes":{"verify":true},"expected":{"verify":null}}`, true},
		{"settings no-op", "PATCH", "/api/workbooks/t11-ua/columns/a/settings", `{"changes":{"verify":true},"expected":{"verify":true}}`, false},
		{"order changes", "PATCH", "/api/workbooks/t11-ua/columns/order", `{"column_ids":["b","a"],"expected_column_ids":["a","b"]}`, true},
		{"order no-op", "PATCH", "/api/workbooks/t11-ua/columns/order", `{"column_ids":["b","a"],"expected_column_ids":["b","a"]}`, false},
		{"width changes", "PATCH", "/api/workbooks/t11-ua/columns/a/width", `{"width":250}`, true},
		{"width no-op", "PATCH", "/api/workbooks/t11-ua/columns/a/width", `{"width":250}`, false},
		{"add", "POST", "/api/workbooks/t11-ua/columns", `{"column":{"id":"c","name":"C"}}`, true},
		{"delete", "DELETE", "/api/workbooks/t11-ua/columns/c", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e.exec(t, "t11-ws", `UPDATE workbooks SET updated_at='2026-01-01 00:00:00' WHERE id='t11-ua'`)
			code, body := e.ed(c.method, c.path, c.body)
			if code != 200 {
				t.Fatalf("%d %s", code, body)
			}
			_, u := e.cols(t, "t11-ws", "t11-ua")
			if moved := u.After(time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)); moved != c.advances {
				t.Fatalf("updated_at advanced=%v want %v (%v)", moved, c.advances, u)
			}
		})
	}
	// Other workbooks keep their stamp.
	if _, u := e.cols(t, "t11-ws", "t11-ub"); !u.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("untouched workbook stamp moved: %v", u)
	}
}

func TestResponsesDoNotHTMLEscape(t *testing.T) {
	e := newColEnv(t)
	e.ownWS(t)
	e.mkWB(t, "t11-ws", "t11-esc", `[]`)
	code, body := e.ed("POST", "/api/workbooks/t11-esc/columns", `{"column":{"id":"a","name":"A","prompt":"<b>x</b> & é \u2028"}}`)
	if code != 200 || !strings.Contains(body, `"prompt":"<b>x</b> & é \u2028"`) {
		t.Fatalf("%d %s", code, body)
	}
}

func TestForeignAndMissingWorkbook404(t *testing.T) {
	e := newColEnv(t)
	e.seedSeed(t)
	for _, c := range []struct{ method, path, body string }{
		{"PATCH", "/api/workbooks/wb-beta-1/columns/a/settings", `{"changes":{"verify":true},"expected":{"verify":null}}`},
		{"PATCH", "/api/workbooks/wb-beta-1/columns/order", `{"column_ids":[],"expected_column_ids":[]}`},
		{"PATCH", "/api/workbooks/wb-beta-1/columns/a/width", `{"width":100}`},
		{"POST", "/api/workbooks/wb-beta-1/columns", `{"column":{"id":"n","name":"N"}}`},
		{"DELETE", "/api/workbooks/wb-beta-1/columns/a", ""},
	} {
		code, body := e.call("alice", "ws-alpha", c.method, c.path, c.body)
		if code != 404 || body != `{"detail":"Workbook not found"}` {
			t.Errorf("%s %s: %d %s", c.method, c.path, code, body)
		}
	}
	if cols, _ := e.cols(t, "ws-beta", "wb-beta-1"); cols != "[]" {
		t.Fatalf("foreign workbook written: %s", cols)
	}
}
