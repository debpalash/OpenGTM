//go:build integration

package runplan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/vendorcatalog"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const testWS = "ws-runplan-test"

var errRollback = errors.New("rollback")

// inTx runs fn in a workspace transaction that is always rolled back, so a
// test seeds freely and leaves nothing behind.
func inTx(t *testing.T, fn func(ctx context.Context, tx pgx.Tx)) {
	t.Helper()
	pool := testpg.NewAppPool(t, 4)
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, pool, testWS, func(tx pgx.Tx) error {
		fn(ctx, tx)
		return errRollback
	})
	if err != nil && !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
}

func mustExec(t *testing.T, ctx context.Context, tx pgx.Tx, sql string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func seedWorkbook(t *testing.T, ctx context.Context, tx pgx.Tx, id, sourceType, filter string, cols json.RawMessage) {
	t.Helper()
	mustExec(t, ctx, tx, `INSERT INTO workbooks (id, name, description, status, workspace_id, source_type, source_config,
		filter_criteria, columns_config, total_rows, completed_rows, sync_to_leads)
		VALUES ($1, $1, '', 'idle', $2, $3, '{}'::json, $4::json, $5::json, 0, 0, false)`, id, testWS, sourceType, filter, string(cols))
}

func seedRow(t *testing.T, ctx context.Context, tx pgx.Tx, id int, wb string, pos int, data, enr string, lead *int) {
	t.Helper()
	mustExec(t, ctx, tx, `INSERT INTO workbook_rows (id, workbook_id, "position", data, enrichments, lead_id, workspace_id)
		VALUES ($1,$2,$3,$4::json,$5::json,$6,$7)`, id, wb, pos, data, enr, lead, testWS)
}

// seedContract recreates the Python contract seed's enrichment workbooks
// (scripts/contract_seed.py _seed_enrichment) that the run fixtures target.
func seedContract(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	s := loadSeed(t)
	seedWorkbook(t, ctx, tx, "wb-enrich-1", "manual", "{}", s.Columns)
	seedWorkbook(t, ctx, tx, "wb-enrich-big", "manual", "{}", s.BigCols)
	seedWorkbook(t, ctx, tx, "wb-enrich-v1", "leads_filter", `{"city":"Berlin"}`, s.V1Columns)
	seedWorkbook(t, ctx, tx, "wb-alpha-1", "manual", "{}", json.RawMessage(`[{"id":"company","name":"Company","type":"lead_field","lead_field":"company"}]`))
	sites := []string{"https://globex.example.com", "https://initech.example.com", "", "https://umbrella.example.com", "https://hooli.example.com", "https://stark.example.com"}
	companies := []string{"Globex", "Initech", "Soylent", "Umbrella", "Hooli", "Stark Industries"}
	for k := 0; k < 6; k++ {
		email := fmt.Sprintf("hello@%s.example.com", strings.ToLower(strings.Fields(companies[k])[0]))
		if k%2 == 1 {
			email = ""
		}
		data, _ := json.Marshal(map[string]any{"company": companies[k], "website": sites[k], "email": email})
		enr := fmt.Sprintf(`{"calc_len":{"value":"%d","status":"complete","provider":"formula","error":null}}`, len(companies[k]))
		seedRow(t, ctx, tx, 101+k, "wb-enrich-1", k, string(data), enr, nil)
	}
	for k := 0; k < 250; k++ {
		seedRow(t, ctx, tx, 1001+k, "wb-enrich-big", k, fmt.Sprintf(`{"company":"Bulk %03d"}`, k+1), `{}`, nil)
	}
	mustExec(t, ctx, tx, `INSERT INTO workbook_views (id, workspace_id, workbook_id, name, config)
		VALUES ('view-enrich-1',$1,'wb-enrich-1','Has website',$2::json)`, testWS,
		`{"filters":[{"column":"website","op":"not_empty","value":null}],"hidden_columns":[]}`)
}

func loadWorkbook(t *testing.T, ctx context.Context, tx pgx.Tx, id string) Workbook {
	t.Helper()
	var name, st string
	var srcType *string
	var cols, filter []byte
	err := tx.QueryRow(ctx, `SELECT name, status, source_type, columns_config, filter_criteria FROM workbooks WHERE id=$1`, id).Scan(&name, &st, &srcType, &cols, &filter)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := wbcrud.ParseColumns(cols)
	if err != nil {
		t.Fatal(err)
	}
	wb := Workbook{ID: id, SourceType: *srcType, SourceConfig: wbcrud.NewObj(), Columns: parsed}
	if len(filter) > 0 {
		doc, err := wbcrud.ParseObject(filter)
		if err != nil {
			t.Fatal(err)
		}
		wb.FilterCriteria = doc
	}
	return wb
}

type fakeLeads struct {
	seen  leadsread.Filter
	leads []leadsread.Lead
}

func (f *fakeLeads) QueryLeads(_ context.Context, _ pgx.Tx, flt leadsread.Filter, page, size int) ([]leadsread.Lead, int, error) {
	f.seen = flt
	return f.leads, len(f.leads), nil
}
func (f *fakeLeads) LeadsByIDs(context.Context, pgx.Tx, []int64) ([]leadsread.Lead, error) {
	return nil, nil
}

func researchOn(t *testing.T) {
	t.Helper()
	vendorcatalog.SetResearchFlag(func() bool { return true })
	Wire(WireDeps{Index: func() map[string]manifest.Entry { return nil },
		AppSetting: func(context.Context, string, string) (string, error) { return "1", nil }})
	t.Cleanup(func() { resetWiringForTest() })
}

type fixture struct {
	Request struct {
		Body json.RawMessage `json:"body"`
		Path string          `json:"path"`
	} `json:"request"`
	Response struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	} `json:"response"`
}

// fixtureProjected is the platform charge the recorded run debited, derived
// from the recorded balance chain (10.00 -> -0.30 -> -0.05 -> -5.00 = 4.65).
var fixtureProjected = map[string]float64{
	"run_billing_ok_debit":          0.3,
	"run_billing_cell_scope_counts": 0.05,
	"run_billing_partial_rows":      5.0,
	"run_billing_byok_no_debit":     0,
	"run_billing_skipped_no_debit":  0,
}

// TestBuildMatchesRunFixtures replays every FastAPI-recorded POST /run
// fixture whose outcome the planner decides (everything past authentication,
// workbook lookup and the debit itself) and compares status, detail, message
// and total_jobs.
func TestBuildMatchesRunFixtures(t *testing.T) {
	files, err := filepath.Glob("../../testdata/contract/run/run_*.json")
	if err != nil || len(files) < 30 {
		t.Fatalf("fixtures: %d %v", len(files), err)
	}
	researchOn(t)
	leads := &fakeLeads{leads: []leadsread.Lead{{ID: 301}, {ID: 302}, {ID: 303}}}
	skip := map[string]bool{"run_viewer": true, "run_foreign": true, "run_unknown": true, "run_no_token": true, "run_billing_viewer": true}
	inTx(t, func(ctx context.Context, tx pgx.Tx) {
		seedContract(t, ctx, tx)
		replayed := 0
		for _, f := range files {
			name := strings.TrimSuffix(filepath.Base(f), ".json")
			if skip[name] {
				continue
			}
			raw, _ := os.ReadFile(f)
			var fx fixture
			if err := json.Unmarshal(raw, &fx); err != nil {
				t.Fatal(err)
			}
			wbID := strings.Split(fx.Request.Path, "/")[3]
			t.Run(name, func(t *testing.T) {
				want, _ := canonjson.Decode(fx.Response.Body)

				body := fx.Request.Body
				req, ferrs := ParseRunRequest(body)
				if ferrs != nil {
					got := mustDecode(t, map[string]any{"detail": ferrs})
					if fx.Response.Status != 422 || !reflect.DeepEqual(got, want) {
						t.Fatalf("field errors\n got %s\nwant %d %s", mustJSON(map[string]any{"detail": ferrs}), fx.Response.Status, fx.Response.Body)
					}
					return
				}
				wb := loadWorkbook(t, ctx, tx, wbID)
				cfg := Config{WorkspaceID: testWS, Leads: leads, DefaultConcurrency: 12,
					Perf: Perf{RowConcurrency: 12, MaxProviders: 0, RetryPasses: 1, ProviderWorkers: 4, ProviderTimeout: 10}}
				plan, err := Build(ctx, tx, wb, req, cfg)
				var status int
				var got map[string]any
				switch {
				case err != nil:
					g := guard(t, err)
					status, got = g.Status, map[string]any{"detail": g.Detail}
				case plan.Skipped:
					status, got = 200, map[string]any{"status": "skipped", "total_jobs": int64(0), "message": plan.Message}
				default:
					status, got = 200, map[string]any{"status": "started", "total_jobs": int64(plan.TotalJobs), "message": plan.Message}
				}
				if status != fx.Response.Status {
					t.Fatalf("status %d want %d (%s)", status, fx.Response.Status, fx.Response.Body)
				}
				wm := want.(map[string]any)
				if status == 402 {
					t.Skip("402 is the debit's answer; required_usd checked below")
				}
				for k, v := range got {
					if wm[k] != v {
						t.Errorf("%s: got %v want %v", k, v, wm[k])
					}
				}
				if p, ok := fixtureProjected[name]; ok && plan.ProjectedUSD != p {
					t.Errorf("projected %v want %v", plan.ProjectedUSD, p)
				}
			})
			replayed++
		}
		if replayed < 33 {
			t.Fatalf("only %d fixtures replayed", replayed)
		}
		// 402 fixtures: the projected cost is the recorded required_usd.
		for _, c := range []struct {
			name string
			want float64
		}{{"run_billing_402", 12.5}, {"run_billing_second_402", 5.0}} {
			raw, _ := os.ReadFile("../../testdata/contract/run/" + c.name + ".json")
			var fx fixture
			_ = json.Unmarshal(raw, &fx)
			req, _ := ParseRunRequest(fx.Request.Body)
			plan, err := Build(ctx, tx, loadWorkbook(t, ctx, tx, "wb-enrich-big"), req, Config{WorkspaceID: testWS})
			if err != nil || plan.ProjectedUSD != c.want {
				t.Errorf("%s: projected %v err %v want %v", c.name, plan.ProjectedUSD, err, c.want)
			}
		}
	})
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func mustDecode(t *testing.T, v any) any {
	t.Helper()
	out, err := canonjson.Decode([]byte(mustJSON(v)))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRowSetResolvedOnce: a view or search is resolved in the planner; the
// payload carries the exact row ids and no filter for the worker to re-derive.
func TestRowSetResolvedOnce(t *testing.T) {
	inTx(t, func(ctx context.Context, tx pgx.Tx) {
		seedContract(t, ctx, tx)
		wb := loadWorkbook(t, ctx, tx, "wb-enrich-1")
		for _, c := range []struct {
			name string
			req  RunRequest
			want []int64
		}{
			{"view", RunRequest{ColumnIDs: []string{"calc_len"}, ViewID: "view-enrich-1"}, []int64{101, 102, 104, 105, 106}},
			{"search", RunRequest{ColumnIDs: []string{"calc_len"}, Search: " STARK "}, []int64{106}},
			{"all", RunRequest{ColumnIDs: []string{"calc_len"}}, []int64{101, 102, 103, 104, 105, 106}},
			{"row_ids win over search", RunRequest{ColumnIDs: []string{"calc_len"}, RowIDs: []int64{103, 101, 999}, Search: "stark"}, []int64{101, 103}},
		} {
			plan, err := Build(ctx, tx, wb, c.req, Config{WorkspaceID: testWS})
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if !reflect.DeepEqual(plan.Rows, c.want) {
				t.Errorf("%s: rows %v want %v", c.name, plan.Rows, c.want)
			}
			var p map[string]json.RawMessage
			if err := json.Unmarshal(plan.Payload, &p); err != nil {
				t.Fatal(err)
			}
			var got []int64
			_ = json.Unmarshal(p["row_ids"], &got)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s: payload row_ids %v want %v", c.name, got, c.want)
			}
			for _, k := range []string{"view_id", "search", "expected_rows"} {
				if _, ok := p[k]; ok {
					t.Errorf("%s: payload must not carry %s", c.name, k)
				}
			}
			if string(p["lead_ids"]) != "null" {
				t.Errorf("%s: lead_ids %s", c.name, p["lead_ids"])
			}
		}
	})
}

func keyOrder(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object")
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

// TestPayloadJobShape: the job payload has Python's keys in Python's order,
// the row_columns scope restricted to the resolved rows in row order.
func TestPayloadJobShape(t *testing.T) {
	inTx(t, func(ctx context.Context, tx pgx.Tx) {
		seedContract(t, ctx, tx)
		wb := loadWorkbook(t, ctx, tx, "wb-enrich-1")
		cfg := Config{WorkspaceID: testWS, DefaultConcurrency: 12,
			Perf: Perf{RowConcurrency: 9, MaxProviders: 2, RetryPasses: 3, ProviderWorkers: 5, ProviderTimeout: 30}}
		plan, err := Build(ctx, tx, wb, RunRequest{
			ColumnIDs: []string{"calc_len", "out_hook"}, RowIDs: []int64{102, 101},
			RowColumns:  map[string][]string{"102": {"out_hook", "calc_len"}, "101": {"calc_len"}},
			FillMissing: true, Force: true}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"workbook_id", "workspace_id", "run_id", "column_ids", "row_columns", "row_ids", "lead_ids",
			"concurrency", "max_providers", "retry_passes", "provider_workers", "provider_timeout", "fill_missing", "force"}
		if got := keyOrder(t, plan.Payload); !reflect.DeepEqual(got, want) {
			t.Fatalf("keys %v\nwant %v", got, want)
		}
		raw := plan.Payload
		var q map[string]any
		_ = json.Unmarshal(raw, &q)
		if q["workbook_id"] != "wb-enrich-1" || q["workspace_id"] != testWS || q["run_id"] != plan.RunID || len(plan.RunID) != 32 {
			t.Fatalf("identity fields: %v", q)
		}
		// resolved rows come back in position order; calc_len runs before out_hook (output last).
		if got := string(plan.Payload[bytes.Index(plan.Payload, []byte(`"row_columns"`)):][:len(`"row_columns":{"101":["calc_len"],"102":["out_hook","calc_len"]}`)]); got != `"row_columns":{"101":["calc_len"],"102":["out_hook","calc_len"]}` {
			t.Fatalf("row_columns: %s", got)
		}
		for k, v := range map[string]float64{"concurrency": 9, "max_providers": 2, "retry_passes": 3, "provider_workers": 5, "provider_timeout": 30} {
			if q[k] != v {
				t.Errorf("%s = %v want %v", k, q[k], v)
			}
		}
		if q["fill_missing"] != true || q["force"] != true {
			t.Errorf("flags %v %v", q["fill_missing"], q["force"])
		}
		if plan.TotalJobs != 3 || plan.MatchedRows != 2 {
			t.Errorf("total %d rows %d", plan.TotalJobs, plan.MatchedRows)
		}
		if !reflect.DeepEqual(ids(plan.Columns), []string{"calc_len", "out_hook"}) {
			t.Errorf("columns %v", ids(plan.Columns))
		}
	})
}

func TestV1LegacyPlan(t *testing.T) {
	inTx(t, func(ctx context.Context, tx pgx.Tx) {
		seedContract(t, ctx, tx)
		wb := loadWorkbook(t, ctx, tx, "wb-enrich-v1")
		leads := &fakeLeads{leads: []leadsread.Lead{{ID: 301}, {ID: 302}, {ID: 303}}}
		cfg := Config{WorkspaceID: testWS, Leads: leads}
		plan, err := Build(ctx, tx, wb, RunRequest{ColumnIDs: []string{"calc_len"}, LeadIDs: []int64{303, 301, 999}}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if leads.seen.City != "Berlin" {
			t.Fatalf("workbook filter not applied: %+v", leads.seen)
		}
		if !reflect.DeepEqual(plan.LeadIDs, []int64{301, 303}) || plan.Rows != nil || plan.TotalJobs != 2 {
			t.Fatalf("%+v", plan)
		}
		var q map[string]any
		_ = json.Unmarshal(plan.Payload, &q)
		if q["row_ids"] != nil || !reflect.DeepEqual(q["lead_ids"], []any{float64(301), float64(303)}) {
			t.Fatalf("payload %s", plan.Payload)
		}
		// an explicit row selection never falls back to legacy leads
		plan, err = Build(ctx, tx, wb, RunRequest{ColumnIDs: []string{"calc_len"}, RowIDs: []int64{1}}, cfg)
		if err != nil || !plan.Skipped || plan.Message != "No rows to process" {
			t.Fatalf("%+v %v", plan, err)
		}
	})
}

func TestExpectedRowsAndScopeDrift409(t *testing.T) {
	inTx(t, func(ctx context.Context, tx pgx.Tx) {
		seedContract(t, ctx, tx)
		wb := loadWorkbook(t, ctx, tx, "wb-enrich-1")
		five, six := 5, 6
		_, err := Build(ctx, tx, wb, RunRequest{ColumnIDs: []string{"calc_len"}, ExpectedRows: &five}, Config{WorkspaceID: testWS})
		if g := guard(t, err); g.Status != 409 || g.Detail != "The matching row count changed. Refresh the run review before starting." {
			t.Fatalf("%v", g)
		}
		if _, err := Build(ctx, tx, wb, RunRequest{ColumnIDs: []string{"calc_len"}, ExpectedRows: &six}, Config{WorkspaceID: testWS}); err != nil {
			t.Fatal(err)
		}
		// row_columns naming a row that does not exist: the resolved set differs
		_, err = Build(ctx, tx, wb, RunRequest{ColumnIDs: []string{"calc_len"}, RowIDs: []int64{101, 999},
			RowColumns: map[string][]string{"101": {"calc_len"}, "999": {"calc_len"}}}, Config{WorkspaceID: testWS})
		if g := guard(t, err); g.Status != 409 || g.Detail != "Selected workbook rows changed. Refresh before running." {
			t.Fatalf("%v", g)
		}
	})
}

// TestExecutorGoOnlyInCohort: the go block and executor "go" appear only for
// an eligible run of a cohort workspace with the flag on.
func TestExecutorGoOnlyInCohort(t *testing.T) {
	inTx(t, func(ctx context.Context, tx pgx.Tx) {
		seedWorkbook(t, ctx, tx, "wb-go", "manual", "{}", json.RawMessage(`[
			{"id":"mob","name":"Mobile","type":"enrichment","provider":"prospeo_mobile","target_field":"mobile_phone"},
			{"id":"cl","name":"Calc","type":"formula","formula":"1"}]`))
		seedRow(t, ctx, tx, 5001, "wb-go", 0, `{"company":"A"}`, `{}`, nil)
		wb := loadWorkbook(t, ctx, tx, "wb-go")
		idx := realIndex(t)
		base := Config{WorkspaceID: testWS, Index: idx, GoExecutorEnabled: true, GoCohort: map[string]bool{testWS: true}, DefaultConcurrency: 12,
			Cost: func(n string) float64 { return idx[n].Manifest.CostPerLookup }}
		run := func(cfg Config, cols ...string) Plan {
			p, err := Build(ctx, tx, wb, RunRequest{ColumnIDs: cols}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			return p
		}
		p := run(base, "mob")
		if p.Executor != "go" || p.Reason != "ok" || !bytes.Contains(p.Payload, []byte(`"go":{"version":1,"manifests":{"prospeo_mobile":`)) {
			t.Fatalf("eligible: %s %s %s", p.Executor, p.Reason, p.Payload)
		}
		off := base
		off.GoExecutorEnabled = false
		if p := run(off, "mob"); p.Executor != "python" || p.Reason != "flag_off" || bytes.Contains(p.Payload, []byte(`"go"`)) {
			t.Fatalf("flag off: %s %s %s", p.Executor, p.Reason, p.Payload)
		}
		out := base
		out.GoCohort = map[string]bool{"someone-else": true}
		if p := run(out, "mob"); p.Executor != "python" || p.Reason != "not_in_cohort" || bytes.Contains(p.Payload, []byte(`"go"`)) {
			t.Fatalf("not in cohort: %s %s", p.Executor, p.Reason)
		}
		if p := run(base, "mob", "cl"); p.Executor != "python" || p.Reason != "column_type" {
			t.Fatalf("formula column: %s %s", p.Executor, p.Reason)
		}
	})
}
