//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const (
	rowsWsAlpha = "t12-ws-alpha"
	rowsWsBeta  = "t12-ws-beta"
)

// rowsCaseOrder is the order contract_cases/rows.yaml was recorded in: later
// cases depend on the writes of earlier ones.
var rowsCaseOrder = []string{
	"add_no_token", "add_non_editor", "add_unknown_workbook", "add_foreign_workbook", "add_empty_rows",
	"add_missing_rows", "add_rows_not_dicts", "add_dedupe_not_bool", "add_body_not_object", "add_no_body",
	"add_ok_dedupe", "add_dedupe_off", "add_dedupe_again", "add_existing_seeded", "add_as_editor",
	"delete_no_token", "delete_non_editor", "delete_empty", "delete_bad_ids", "delete_unknown_workbook",
	"delete_foreign_ids", "delete_ok", "delete_again", "dq_no_token", "dq_non_editor", "dq_invalid",
	"dq_missing", "dq_unknown_workbook", "dq_unknown_view", "dq_drift", "dq_confirmation_mismatch",
	"dq_nothing_matches", "dq_ok_view", "dq_ok_search", "dq_ok_again_drift", "row_no_token", "row_non_editor",
	"row_ok", "row_unchanged", "row_false_then_zero", "row_zero_after_false", "row_unknown_row",
	"row_other_workbook", "row_unknown_workbook", "row_no_fields", "row_empty_body", "row_protected_fields",
	"row_derived_column_not_editable", "row_body_not_object", "row_no_body", "row_bad_id", "row_bad_recompute",
	"row_bad_id_and_recompute", "row_recompute_false", "row_nested_value", "row_nested_reordered",
	"bulk_no_token", "bulk_non_editor", "bulk_ok", "bulk_ok_unchanged", "bulk_duplicate_ids", "bulk_foreign_id",
	"bulk_row_of_other_workbook", "bulk_non_editable_field", "bulk_empty_fields", "bulk_unknown_workbook",
	"bulk_empty_updates", "bulk_missing_updates", "bulk_bad_items", "bulk_1001_updates",
	"bulk_1000_updates_unknown_ids", "lead_no_token", "lead_non_editor", "lead_ok", "lead_no_valid_fields",
	"lead_empty_body", "lead_not_found", "lead_other_workspace", "lead_unknown_workbook",
	"lead_foreign_workbook", "lead_body_not_object", "lead_bad_id",
}

type rowsEnv struct {
	fnEnv
	owner *pgxpool.Pool
}

func newRowsEnv(t *testing.T) rowsEnv {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	pool := testpg.NewAppPool(t, 4)
	owner, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	RegisterWorkbookRows(r, Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Flags: config.Flags{PGLeadStore: true}})
	e := rowsEnv{fnEnv: fnEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}, owner: owner}
	e.clean()
	t.Cleanup(e.clean)
	return e
}

func (e rowsEnv) clean() {
	ctx := context.Background()
	for _, sql := range []string{
		`DELETE FROM jobs WHERE type='run_workbook' AND payload->>'workbook_id' IN ('wb-alpha-1','wb-alpha-2','wb-beta-1')`,
		`DELETE FROM workbooks WHERE id IN ('wb-alpha-1','wb-alpha-2','wb-beta-1')`,
		`DELETE FROM leads WHERE workspace_id IN ('` + rowsWsAlpha + `','` + rowsWsBeta + `')`,
	} {
		_, _ = e.owner.Exec(ctx, sql)
	}
}

const alpha2Columns = `[{"id": "company", "name": "Company", "type": "lead_field", "width": 200, "lead_field": "company"},
 {"id": "website", "name": "Website", "type": "lead_field", "width": 200, "lead_field": "website"},
 {"id": "email", "name": "Email", "type": "lead_field", "width": 220, "lead_field": "email"},
 {"id": "city", "name": "City", "type": "lead_field", "width": 150, "lead_field": "city"},
 {"id": "phone", "name": "Phone", "type": "lead_field", "width": 150, "lead_field": "phone"},
 {"id": "email_wf", "name": "Email Finder", "type": "waterfall", "width": 220, "waterfall": ["hunter", "apollo"], "target_field": "email", "verify": true},
 {"id": "summary", "name": "Summary", "type": "ai_formula", "width": 300, "prompt": "Summarize {Company} in one sentence", "input_columns": ["company"]}]`

func pyObj(kv ...any) *wbcrud.Obj {
	o := wbcrud.NewObj()
	for i := 0; i < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

func two(i int) string { return string(rune('0'+i/10)) + string(rune('0'+i%10)) }

// seed mirrors contract_seed.py's workbook plane (the state every fixture was recorded against).
func (e rowsEnv) seed(t *testing.T) (idBase int64) {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.owner.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	for _, w := range []struct {
		id, ws, name, desc, cols string
		total                    int
	}{
		{"wb-alpha-1", rowsWsAlpha, "Alpha Empty", "empty workbook", "[]", 0},
		{"wb-alpha-2", rowsWsAlpha, "Alpha Leads", "25 seeded rows", alpha2Columns, 25},
		{"wb-beta-1", rowsWsBeta, "Beta Workbook", "beta tenant", "[]", 0},
	} {
		exec(`INSERT INTO workbooks (id, name, description, status, workspace_id, source_type, source_config, filter_criteria, columns_config,
			total_rows, completed_rows, sync_to_leads, budget_spent_usd, created_at, updated_at)
			VALUES ($1,$2,$3,'idle',$4,'manual','{}'::json,'{}'::json,$5::json,$6,0,false,0,'2026-08-29 10:00:00','2026-08-29 10:00:00')`,
			w.id, w.name, w.desc, w.ws, w.cols, w.total)
	}
	cities := []string{"Berlin", "Austin", "Pune", "Lyon", "Oslo"}
	for i := 1; i <= 25; i++ {
		var email, city any = "info@acme" + two(i) + ".example.com", cities[i%5]
		website := "https://acme" + two(i) + ".example.com"
		if i%7 == 0 {
			email = nil
		}
		if i%9 == 0 {
			website = ""
		}
		if i%11 == 0 {
			city = nil
		}
		data := pyObj("company", "Acme "+two(i), "website", website, "email", email, "city", city, "phone", "+1-555-01"+two(i))
		var ev, prov, errv any = "found" + two(i) + "@acme" + two(i) + ".example.com", "hunter", nil
		st := "complete"
		if i%4 == 0 {
			ev, st, prov, errv = nil, "failed", nil, "no result"
		}
		var sv any = "Acme " + two(i) + " sells widgets."
		sst := "complete"
		if i%3 == 0 {
			sv, sst = nil, "pending"
		}
		enr := pyObj("email_wf", pyObj("value", ev, "status", st, "provider", prov, "error", errv),
			"summary", pyObj("value", sv, "status", sst, "provider", "llm", "error", nil))
		exec(`INSERT INTO workbook_rows (id, workbook_id, position, data, enrichments, created_at, updated_at, workspace_id)
			VALUES ($1,'wb-alpha-2',$2,$3::json,$4::json,'2026-08-29 10:00:00','2026-08-29 10:00:00',$5)`,
			i, i-1, wbcrud.PyDumps(data, false), wbcrud.PyDumps(enr, false), rowsWsAlpha)
	}
	// never rewind the shared sequence (other tests' rows keep their ids); new rows then get
	// ids idBase+1.., which stateOf maps back to FastAPI's 26.. for the comparison
	if err := e.owner.QueryRow(ctx, `SELECT setval(pg_get_serial_sequence('workbook_rows','id'), greatest(25, (SELECT last_value FROM workbook_rows_id_seq)))`).Scan(&idBase); err != nil {
		t.Fatal(err)
	}
	for _, v := range [][3]string{
		{"view-alpha-1", "Berlin only", `{"filters": [{"column": "city", "op": "equals", "value": "Berlin"}], "hidden_columns": []}`},
		{"view-alpha-2", "Sorted by company", `{"sort": [{"column": "company", "dir": "asc"}], "hidden_columns": ["phone"]}`},
	} {
		exec(`INSERT INTO workbook_views (id, workspace_id, workbook_id, name, config, created_at, updated_at)
			VALUES ($1,$2,'wb-alpha-2',$3,$4::json,'2026-08-29 10:00:00','2026-08-29 10:00:00')`, v[0], rowsWsAlpha, v[1], v[2])
	}
	for _, l := range []struct {
		id                          int
		ws, company, site, em, city string
	}{
		{1, rowsWsAlpha, "Acme 01", "https://acme01.example.com", "info@acme01.example.com", "Austin"},
		{2, rowsWsAlpha, "Globex", "https://globex.example.com", "hello@globex.example.com", "Berlin"},
		{3, rowsWsBeta, "Initech", "https://initech.example.com", "team@initech.example.com", "Pune"},
	} {
		exec(`INSERT INTO leads (id, workspace_id, company, website, email, city, status, source, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,'new','contract','2026-08-29T10:00:00','2026-08-29T10:00:00')`, l.id, l.ws, l.company, l.site, l.em, l.city)
	}
	return idBase
}

// users: alice admin, erin editor, bob a plain member of the alpha workspace.
func (e rowsEnv) users(t *testing.T) map[string]map[string]string {
	t.Helper()
	e.user(t, rowsWsBeta, "t12-beta-owner", "admin", false) // creates the beta workspace
	return map[string]map[string]string{
		"alice": e.user(t, rowsWsAlpha, "t12-alice", "admin", false),
		"erin":  e.user(t, rowsWsAlpha, "t12-erin", "editor", false),
		"bob":   e.user(t, rowsWsAlpha, "t12-bob", "viewer", false),
	}
}

// bodyOverrides keeps the original key order of bodies whose response or stored
// data depends on it (the recorder sorts keys when it writes fixtures).
var bodyOverrides = map[string]string{
	"add_ok_dedupe":        `{"rows":[{"company":"Zed Corp","website":"https://www.zedco.io/x"},{"domain":"ZEDCO.io","company":"Other"},{"company":"Zed Corp"},{"company":"Zed, Inc."},{"notes":"a"},{"notes":"b"},{"company":"Caf\u00e9 M\u00fcnch GmbH"},{"company":"Cafe Munch"},{"id":5,"company":"Has Id","score":1.5,"ratio":100.0},{"website":"https://shop.acme.co.uk/a"},{"website":"http://other.acme.co.uk"},{"website":"Zedco.IO:8080"}]}`,
	"row_ok":               `{"phone": "+1-999", "city": "Rome", "score": 0}`,
	"row_unchanged":        `{"phone": "+1-999", "city": "Rome"}`,
	"row_recompute_false":  `{"company": "Renamed Co", "city": "Paris"}`,
	"row_nested_value":     `{"notes": {"b": [1, 2.5, null], "a": "\u00e9"}}`,
	"row_nested_reordered": `{"notes": {"a": "\u00e9", "b": [1, 2.5, null]}}`,
	"bulk_ok":              `{"updates":[{"row_id":6,"fields":{"city":"Oslo"}},{"row_id":7,"fields":{"phone":"1","city":"Oslo","score":3}}]}`,
}

func whoIs(name string) string {
	switch {
	case strings.Contains(name, "no_token"):
		return ""
	case strings.Contains(name, "non_editor"):
		return "bob"
	case name == "add_as_editor":
		return "erin"
	}
	return "alice"
}

func (e rowsEnv) replay(t *testing.T, area, name string, users map[string]map[string]string, opts ...contract.Option) {
	t.Helper()
	fx := contract.Load(t, area+"/"+name)
	body := string(fx.Request.Body)
	if body == "null" {
		body = ""
	}
	if o, ok := bodyOverrides[name]; ok {
		body = o
	}
	_, got := e.do(fx.Request.Method, fx.Request.Path, "application/json", body, users[whoIs(name)])
	if name == "bulk_1001_updates" {
		// F6: the recorded Python body echoes all 1001 updates; Go answers "<omitted>".
		t.Run(name, func(t *testing.T) {
			b := string(got.Body)
			if got.Status != 422 || !strings.Contains(b, `"too_long"`) || !strings.Contains(b, `"input":"<omitted>"`) || len(b) > 1000 {
				t.Fatalf("%d %.300s", got.Status, b)
			}
		})
		return
	}
	t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response, opts...) })
}

// stateOf is the same projection legacy/python dumped after recording rows.yaml.
func (e rowsEnv) stateOf(t *testing.T, idBase int64) map[string]any {
	t.Helper()
	var raw []byte
	err := e.owner.QueryRow(context.Background(), fmt.Sprintf(`select json_build_object(
 'rows', (select json_agg(json_build_object('id',CASE WHEN id > 25 THEN id - %d + 25 ELSE id END,'workbook_id',workbook_id,'position',position,'data',data::text,'enrichments',enrichments::text,'lead_id',lead_id,'corroboration_count',corroboration_count) order by id) from workbook_rows where workbook_id in ('wb-alpha-1','wb-alpha-2','wb-beta-1')),
 'workbooks', (select json_agg(json_build_object('id',id,'source_config',source_config::text,'status',status,'total_rows',total_rows) order by id) from workbooks where id in ('wb-alpha-1','wb-alpha-2','wb-beta-1')),
 'leads', (select json_agg(json_build_object('id',id,'company',company,'city',city,'score',score) order by id) from leads where id in (1,2,3)))::text`, idBase)).Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRowsMatchFixtures(t *testing.T) {
	e := newRowsEnv(t)
	idBase := e.seed(t)
	users := e.users(t)
	for _, name := range rowsCaseOrder {
		e.replay(t, "rows", name, users)
	}
	golden, err := os.ReadFile("../../testdata/wbcrud/rows_final_state.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(golden, &want); err != nil {
		t.Fatal(err)
	}
	got := e.stateOf(t, idBase)
	for _, k := range []string{"rows", "workbooks", "leads"} {
		g, _ := got[k].([]any)
		w, _ := want[k].([]any)
		if len(g) != len(w) {
			t.Errorf("final %s: %d entries, FastAPI left %d", k, len(g), len(w))
			continue
		}
		for i := range w {
			if !reflect.DeepEqual(g[i], w[i]) {
				gb, _ := json.Marshal(g[i])
				wb, _ := json.Marshal(w[i])
				t.Errorf("final %s[%d] differs from the FastAPI state:\n got %s\nwant %s", k, i, gb, wb)
			}
		}
	}
}

func TestRowRecomputeEnqueuesRunJob(t *testing.T) {
	e := newRowsEnv(t)
	e.seed(t)
	users := e.users(t)
	for _, name := range []string{"rc_row_company", "rc_row_unchanged", "rc_row_recompute_false", "rc_bulk", "rc_bulk_no_downstream"} {
		e.replay(t, "rows_recompute", name, users, contract.IgnorePaths("$.recompute.job_id", "$.recompute.run_id"))
	}
	rows, err := e.owner.Query(context.Background(), `SELECT payload::text, status, coalesce(error,''), executor, workspace_id, priority, max_retries
		FROM jobs WHERE type='run_workbook' AND payload->>'workbook_id'='wb-alpha-2' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type job struct {
		payload, status, errText, executor, ws string
		prio, retries                          int
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.payload, &j.status, &j.errText, &j.executor, &j.ws, &j.prio, &j.retries); err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, j)
	}
	if len(jobs) != 2 {
		t.Fatalf("want 2 run jobs (single-row run, bulk run), got %d", len(jobs))
	}
	runID := func(p string) string {
		var m map[string]any
		_ = json.Unmarshal([]byte(p), &m)
		s, _ := m["run_id"].(string)
		return s
	}
	for i, want := range []string{
		`{"workbook_id":"wb-alpha-2","workspace_id":"` + rowsWsAlpha + `","run_id":"RUN","column_ids":["summary"],"row_columns":null,"row_ids":[9],"lead_ids":null,"concurrency":12,"max_providers":8,"retry_passes":1,"provider_workers":8,"provider_timeout":10,"fill_missing":false,"force":true}`,
		`{"workbook_id":"wb-alpha-2","workspace_id":"` + rowsWsAlpha + `","run_id":"RUN","column_ids":["summary"],"row_columns":{"13":["summary"],"17":["summary"]},"row_ids":[13,17],"lead_ids":null,"concurrency":12,"max_providers":8,"retry_passes":1,"provider_workers":8,"provider_timeout":10,"fill_missing":false,"force":true}`,
	} {
		j := jobs[i]
		got := strings.ReplaceAll(strings.ReplaceAll(j.payload, " ", ""), runID(j.payload), "RUN")
		if len(runID(j.payload)) != 32 || got != want {
			t.Errorf("job %d payload\n got %s\nwant %s", i, got, want)
		}
		if j.executor != "go" || j.ws != rowsWsAlpha || j.prio != 1 || j.retries != 3 {
			t.Errorf("job %d columns: %+v", i, j)
		}
	}
	if jobs[0].status != "cancelled" || jobs[0].errText != "superseded by a newer run" || jobs[1].status != "pending" {
		t.Errorf("a new run must supersede the in-flight one: %+v", jobs)
	}
	var status string
	var total int
	if err := e.owner.QueryRow(context.Background(), `SELECT status, total_rows FROM workbooks WHERE id='wb-alpha-2'`).Scan(&status, &total); err != nil || status != "running" || total != 2 {
		t.Errorf("workbook run state: %s %d %v", status, total, err)
	}
}
