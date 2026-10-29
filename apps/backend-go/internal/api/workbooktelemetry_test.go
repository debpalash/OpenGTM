//go:build integration

package api

import (
	"context"
	"encoding/json"
	"os"
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
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type telEnv struct {
	colEnv
	actIDs map[int]int // activity id in this database -> id FastAPI assigned (insertion order 1..6)
}

func newTelEnv(t *testing.T) telEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Flags: config.Flags{PGLeadStore: true}}
	RegisterWorkbooks(r, d)
	RegisterWorkbookTelemetry(r, d)
	e := telEnv{colEnv: colEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}}
	e.purgeTel()
	t.Cleanup(e.purgeTel)
	e.seedSeed(t)
	e.seedTelemetry(t)
	e.actIDs = e.activityIDs(t)
	return e
}

// activityIDs maps the serial ids of this (shared) database to the 1..6 FastAPI's fresh database assigned.
func (e telEnv) activityIDs(t *testing.T) map[int]int {
	t.Helper()
	ctx := context.Background()
	m := map[int]int{}
	if err := tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM workbook_activity ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for n := 1; rows.Next(); n++ {
			var id int
			if err := rows.Scan(&id); err != nil {
				return err
			}
			m[id] = n
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return m
}

var activityIDRE = regexp.MustCompile(`\{"id":(\d+),"kind"`)

// normActivity rewrites activity ids in an /activity body to FastAPI's.
func (e telEnv) normActivity(body []byte) []byte {
	return activityIDRE.ReplaceAllFunc(body, func(m []byte) []byte {
		sub := activityIDRE.FindSubmatch(m)
		n, _ := strconv.Atoi(string(sub[1]))
		return []byte(`{"id":` + strconv.Itoa(e.actIDs[n]) + `,"kind"`)
	})
}

func (e telEnv) purgeTel() {
	ctx := context.Background()
	for _, ws := range []string{"ws-alpha", "ws-beta"} {
		_ = tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			for _, tbl := range []string{"workbook_spend_attempts", "workbook_activity", "cell_traces", "jobs"} {
				if _, err := tx.Exec(ctx, "DELETE FROM "+tbl); err != nil {
					return err
				}
			}
			return nil
		})
	}
	e.purge()
}

// telemetryBase mirrors the telemetry rows of legacy/python/scripts/contract_seed.py (_seed_workbooks).
const telemetryBase = `
INSERT INTO workbook_spend_attempts (id, workspace_id, workbook_id, run_id, row_identity, column_id, provider, attempt_key, contract_hash, status, reserved_microusd, settled_microusd, cost_basis, created_at, updated_at) VALUES
 ('spend-1','ws-alpha','wb-alpha-2','run-alpha-1','row-1','email_wf','hunter','attempt-1',repeat('0',64),'settled',2000,1500,'{"unit": "call"}',1788000000,1788000000),
 ('spend-2','ws-alpha','wb-alpha-2','run-alpha-1','row-2','email_wf','hunter','attempt-2',repeat('0',64),'reserved',2000,NULL,'{"unit": "call"}',1788000000,1788000000);
INSERT INTO workbook_activity (workbook_id, kind, message, created_at, workspace_id) VALUES
 ('wb-alpha-2','created','Workbook created','2026-08-29 10:01:00','ws-alpha'),
 ('wb-alpha-2','run','Run started','2026-08-29 10:02:00','ws-alpha'),
 ('wb-alpha-2','run','Run completed','2026-08-29 10:03:00','ws-alpha');
INSERT INTO cell_traces (workbook_id, lead_id, column_id, goal, steps, outcome, total_cost_usd, created_at, workspace_id) VALUES
 ('wb-alpha-2',1,'summary','summarize','[{"step": 1, "action": "search"}]','ok','0.01','2026-08-29 10:00:00','ws-alpha');
INSERT INTO jobs (id, type, payload, status, priority, created_at, retry_count, max_retries, workspace_id) VALUES
 (1,'run_workbook','{"workbook_id": "wb-alpha-2", "workspace_id": "ws-alpha"}','completed',0,'2026-08-29 10:00:00',0,3,'ws-alpha'),
 (2,'run_workbook','{"workbook_id": "wb-alpha-2", "workspace_id": "ws-alpha"}','queued',0,'2026-08-29 10:00:00',0,3,'ws-alpha');
`

var sectionRE = regexp.MustCompile(`(?m)^-- @(ws|super)(?: (\S+))?\s*$`)

// seedTelemetry loads the contract seed's telemetry rows plus testdata/wbcrud/telemetry_extra.sql.
func (e telEnv) seedTelemetry(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	run := func(ws, sql string) {
		if err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, sql); return err }); err != nil {
			t.Fatalf("seed %s: %v", ws, err)
		}
	}
	testpg.FreeIDs(t, "jobs", testpg.Span(1, 12)...) // recorded job ids (here and in telemetry_extra.sql), below testpg.SeqFloor
	run("ws-alpha", telemetryBase)
	raw, err := os.ReadFile("../../testdata/wbcrud/telemetry_extra.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	locs := sectionRE.FindAllStringSubmatchIndex(text, -1)
	for i, l := range locs {
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if text[l[2]:l[3]] == "ws" {
			run(text[l[4]:l[5]], text[l[1]:end])
		}
	}
}

func (e telEnv) get(who, ws, path string) (int, string) { return e.call(who, ws, "GET", path, "") }

func (e telEnv) alice(method, path, body string) (int, string) {
	return e.call("alice", "ws-alpha", method, path, body)
}

const telBase = "/api/workbooks/wb-alpha-2"

func TestTelemetryMatchesFixtures(t *testing.T) {
	e := newTelEnv(t)
	seen := map[string]bool{}
	for _, c := range telemetryReplay {
		fx := contract.Load(t, "telemetry/"+c.name)
		seen["telemetry/"+c.name] = true
		hdr := map[string]string{}
		if c.who != "" {
			hdr = e.hdr(c.who, c.ws)
		}
		_, got := e.do(fx.Request.Method, fx.Request.Path, "application/json", fixtureBody(fx), hdr)
		if strings.HasPrefix(c.name, "activity_") {
			got.Body = e.normActivity(got.Body)
		}
		t.Run(c.name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
	}
	for _, n := range contractFixtureNames(t, "telemetry") {
		if !seen[n] && !strings.HasPrefix(n, "telemetry/token_") {
			t.Errorf("fixture never replayed: %s", n)
		}
	}
}

func TestRunsPaginationAndMasking(t *testing.T) {
	e := newTelEnv(t)
	type run struct {
		JobID      int             `json:"job_id"`
		RetryCount int             `json:"retry_count"`
		Result     json.RawMessage `json:"result"`
	}
	var all []run
	path := telBase + "/runs?limit=2"
	for pages := 0; pages < 20; pages++ {
		code, body := e.get("alice", "ws-alpha", path)
		if code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		var page struct {
			Runs       []run `json:"runs"`
			HasMore    bool  `json:"has_more"`
			NextBefore *int  `json:"next_before_id"`
		}
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Runs...)
		if !page.HasMore {
			if page.NextBefore != nil {
				t.Fatalf("next_before_id on the last page: %s", body)
			}
			break
		}
		if page.NextBefore == nil || *page.NextBefore != page.Runs[len(page.Runs)-1].JobID {
			t.Fatalf("next_before_id must be the last job_id: %s", body)
		}
		path = telBase + "/runs?limit=2&before_id=" + itoa(*page.NextBefore)
	}
	var ids []int
	for _, r := range all {
		ids = append(ids, r.JobID)
	}
	want := []int{12, 11, 10, 9, 8, 4, 3, 2, 1}
	if len(ids) != len(want) {
		t.Fatalf("ids %v want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids %v want %v", ids, want)
		}
	}
	byID := map[int]run{}
	for _, r := range all {
		byID[r.JobID] = r
	}
	// Job 4 failed on attempt 3 (retry_count 2) but the stored result is from attempt 2: hidden.
	if r := byID[4]; r.RetryCount != 2 || string(r.Result) != "null" {
		t.Errorf("job 4 result must be masked: %+v %s", r, r.Result)
	}
	// Job 8 stored its result for retry_count 1 and only the known keys are exposed.
	if r := byID[8]; !strings.HasPrefix(string(r.Result), `{"completed":3,"errors":0,"total":3,"rows":3,"stopped":false,"recorded_at":"2026-08-29T10:09:00+00:00"}`) {
		t.Errorf("job 8 result: %s", r.Result)
	}
	// Job 3 (retry 0, result retry 0) shows; job 9 (no retry_count in the result) is hidden.
	if r := byID[3]; string(r.Result) == "null" {
		t.Errorf("job 3 result must show")
	}
	if r := byID[9]; string(r.Result) != "null" {
		t.Errorf("job 9 result must be masked: %s", r.Result)
	}
}

func TestRunsBodyBytesAndKeyOrder(t *testing.T) {
	e := newTelEnv(t)
	code, body := e.get("alice", "ws-alpha", telBase+"/runs?limit=1&before_id=4")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	want := `{"runs":[{"job_id":3,"run_id":"run-x","status":"running","row_count":3,"column_count":2,"selected_cell_count":6,` +
		`"output_attempts":{"total":6,"awaiting_receipt":1,"succeeded":1,"failed":1,"unknown":3},"fill_missing":true,"force":false,"retry_count":0,"error":null,` +
		`"created_at":"2026-08-29T10:05:00.123456+00:00","started_at":"2026-08-29T10:05:01+00:00","completed_at":"2026-08-29T10:05:02+00:00",` +
		`"last_heartbeat":"2026-08-29T10:05:30.500000+00:00","next_run_at":null,` +
		`"result":{"completed":5,"errors":1,"total":6,"rows":3,"stopped":false,"recorded_at":"2026-08-29T10:05:20+00:00"}}],"has_more":true,"next_before_id":3}`
	if body != want {
		t.Fatalf("\n got %s\nwant %s", body, want)
	}
	code, body = e.get("alice", "ws-alpha", telBase+"/runs?limit=1&before_id=11")
	if code != 200 || !strings.Contains(body, `"job_id":10,"run_id":null,"status":"completed","row_count":1,"column_count":2,"selected_cell_count":1,`) ||
		!strings.Contains(body, `"result":{"completed":1.0,"errors":0,"total":100.0,"rows":[1,{"b":2,"a":"é"}],"stopped":null,"recorded_at":null}`) {
		t.Fatalf("job 10: %d %s", code, body)
	}
}

func TestRunsOnlyThisWorkbookAndTenant(t *testing.T) {
	e := newTelEnv(t)
	_, body := e.get("alice", "ws-alpha", telBase+"/runs?limit=100")
	for _, bad := range []string{`"run_id":"other-wb"`, `"run_id":"other-tenant"`} {
		if strings.Contains(body, bad) {
			t.Errorf("foreign job leaked (%s): %s", bad, body)
		}
	}
	// Another workspace's job names this workbook id in its payload: invisible to ws-alpha, and the
	// workbook itself does not exist for bob in ws-beta.
	if code, body := e.get("bob", "ws-beta", telBase+"/runs"); code != 404 || body != `{"detail":"Workbook not found"}` {
		t.Errorf("foreign workbook: %d %s", code, body)
	}
	// ws-beta sees only its own job of its own workbook.
	_, body = e.get("bob", "ws-beta", "/api/workbooks/wb-beta-1/runs")
	if body != `{"runs":[],"has_more":false,"next_before_id":null}` {
		t.Errorf("beta runs: %s", body)
	}
	// Non-run job types never show.
	if strings.Contains(body, "refresh") {
		t.Errorf("refresh job shown")
	}
	_, body = e.get("alice", "ws-alpha", telBase+"/runs?limit=100")
	if strings.Contains(body, `"job_id":7,`) {
		t.Errorf("refresh_workbook job 7 listed: %s", body)
	}
}

func TestRunsBeforeIDBeyondInt32Is422(t *testing.T) {
	e := newTelEnv(t)
	// FastAPI answers 500 (int4 overflow in the comparison); Go: 422 less_than_equal.
	code, body := e.get("alice", "ws-alpha", telBase+"/runs?before_id=2147483648")
	want := `{"detail":[{"type":"less_than_equal","loc":["query","before_id"],"msg":"Input should be less than or equal to 2147483647","input":"2147483648","ctx":{"le":2147483647}}]}`
	if code != 422 || body != want {
		t.Fatalf("%d %s", code, body)
	}
	code, body = e.get("alice", "ws-alpha", telBase+"/runs?before_id=99999999999999999999&limit=0")
	if code != 422 || !strings.Contains(body, `"loc":["query","before_id"]`) || !strings.Contains(body, `"loc":["query","limit"]`) {
		t.Fatalf("%d %s", code, body)
	}
	if code, _ := e.get("alice", "ws-alpha", telBase+"/runs?before_id=2147483647"); code != 200 {
		t.Fatalf("int32 max must pass: %d", code)
	}
}

func TestActivityLimitBound(t *testing.T) {
	e := newTelEnv(t)
	for q, want := range map[string]int{"": 6, "?limit=500": 6, "?limit=2": 2, "?limit=0": 0, "?limit=1": 1} {
		code, body := e.get("alice", "ws-alpha", telBase+"/activity"+q)
		var out struct {
			Activity []json.RawMessage `json:"activity"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil || code != 200 || len(out.Activity) != want {
			t.Errorf("%q: %d %s", q, code, body)
		}
	}
	code, body := e.get("alice", "ws-alpha", telBase+"/activity?limit=501")
	if code != 422 || !strings.Contains(body, `"less_than_equal"`) || !strings.Contains(body, `"ctx":{"le":500}`) {
		t.Errorf("501: %d %s", code, body)
	}
	// FastAPI has no lower bound: LIMIT -1 is a Postgres error (500). Go: 422 greater_than_equal.
	code, body = e.get("alice", "ws-alpha", telBase+"/activity?limit=-1")
	want := `{"detail":[{"type":"greater_than_equal","loc":["query","limit"],"msg":"Input should be greater than or equal to 0","input":"-1","ctx":{"ge":0}}]}`
	if code != 422 || body != want {
		t.Errorf("-1: %d %s", code, body)
	}
	// Newest first, NULL created_at first (Postgres DESC), key order id, kind, message, created_at.
	_, body = e.get("alice", "ws-alpha", telBase+"/activity?limit=2")
	body = string(e.normActivity([]byte(body)))
	wantTop := `{"activity":[{"id":6,"kind":null,"message":null,"created_at":null},{"id":5,"kind":"signal","message":"Signal with <html> & \"quotes\" é","created_at":"2026-08-29T10:05:00"}]}`
	if body != wantTop {
		t.Errorf("\n got %s\nwant %s", body, wantTop)
	}
	_, body = e.get("alice", "ws-alpha", telBase+"/activity?limit=4")
	if !strings.Contains(body, `"created_at":"2026-08-29T10:04:00.500000"`) {
		t.Errorf("fraction missing: %s", body)
	}
}

func TestTrace404(t *testing.T) {
	e := newTelEnv(t)
	for _, p := range []string{"/rows/1/cells/email_wf/trace", "/rows/999/cells/summary/trace", "/rows/-5/cells/summary/trace", "/rows/0/cells/summary/trace"} {
		if code, body := e.get("alice", "ws-alpha", telBase+p); code != 404 || body != `{"detail":"No trace for this cell"}` {
			t.Errorf("%s: %d %s", p, code, body)
		}
	}
	if code, body := e.get("alice", "ws-alpha", "/api/workbooks/nope/rows/1/cells/summary/trace"); code != 404 || body != `{"detail":"Workbook not found"}` {
		t.Errorf("unknown workbook: %d %s", code, body)
	}
	// A trace of ws-alpha is not reachable through a workbook of ws-beta.
	if code, body := e.get("bob", "ws-beta", "/api/workbooks/wb-beta-1/rows/2/cells/summary/trace"); code != 404 || body != `{"detail":"No trace for this cell"}` {
		t.Errorf("cross workbook: %d %s", code, body)
	}
	// Beyond int4 FastAPI answers 500; Go: 422.
	code, body := e.get("alice", "ws-alpha", telBase+"/rows/2147483648/cells/summary/trace")
	want := `{"detail":[{"type":"less_than_equal","loc":["path","lead_id"],"msg":"Input should be less than or equal to 2147483647","input":"2147483648","ctx":{"le":2147483647}}]}`
	if code != 422 || body != want {
		t.Errorf("huge lead: %d %s", code, body)
	}
	code, body = e.get("alice", "ws-alpha", telBase+"/rows/-2147483649/cells/summary/trace")
	if code != 422 || !strings.Contains(body, `"greater_than_equal"`) {
		t.Errorf("tiny lead: %d %s", code, body)
	}
	// Key order of a 200.
	_, body = e.get("alice", "ws-alpha", telBase+"/rows/2/cells/summary/trace")
	wantTrace := `{"workbook_id":"wb-alpha-2","lead_id":2,"column_id":"summary","goal":"find the CEO email","steps":[{"step":1,"provider":"hunter","success":true,"value":"x@y.z","cost":0.004,"reason":"domain match é"},` +
		`{"step":2,"provider":"apollo","success":false,"value":null,"cost":0.0,"reason":"skipped"}],"outcome":"found","total_cost_usd":{"spent":0.0123}}`
	if body != wantTrace {
		t.Errorf("\n got %s\nwant %s", body, wantTrace)
	}
}

func TestCostAndBudgetBytes(t *testing.T) {
	e := newTelEnv(t)
	_, body := e.get("alice", "ws-alpha", telBase+"/cost")
	want := `{"budget_max_usd":0.0,"budget_spent_usd":0.0,"reserved_usd":0.002,"uncertain_usd":1e-06,"remaining_usd":null,"accounting_basis":"catalog_estimates_and_recorded_charges","unlimited":true}`
	if body != want {
		t.Errorf("\n got %s\nwant %s", body, want)
	}
	code, body := e.alice("PUT", "/api/workbooks/wb-alpha-1/budget", `{"max_usd":12.5}`)
	if code != 200 || body != `{"budget_max_usd":12.5,"budget_spent_usd":1.23456789}` {
		t.Errorf("put: %d %s", code, body)
	}
	_, body = e.get("alice", "ws-alpha", "/api/workbooks/wb-alpha-1/cost")
	want = `{"budget_max_usd":12.5,"budget_spent_usd":1.2346,"reserved_usd":0.0,"uncertain_usd":0.0,"remaining_usd":11.265432,"accounting_basis":"catalog_estimates_and_recorded_charges","unlimited":false}`
	if body != want {
		t.Errorf("\n got %s\nwant %s", body, want)
	}
}

// FastAPI computes `float - Decimal` here and answers 500; Go answers the figure.
func TestCostCappedWithExposureAnswers200NotPython500(t *testing.T) {
	e := newTelEnv(t)
	if code, _ := e.alice("PUT", telBase+"/budget", `{"max_usd":10}`); code != 200 {
		t.Fatal(code)
	}
	code, body := e.get("alice", "ws-alpha", telBase+"/cost")
	want := `{"budget_max_usd":10.0,"budget_spent_usd":0.0,"reserved_usd":0.002,"uncertain_usd":1e-06,"remaining_usd":9.997999,"accounting_basis":"catalog_estimates_and_recorded_charges","unlimited":false}`
	if code != 200 || body != want {
		t.Errorf("%d\n got %s\nwant %s", code, body, want)
	}
}

func (e telEnv) updatedAt(t *testing.T, id string) time.Time {
	t.Helper()
	ctx := context.Background()
	var u time.Time
	if err := tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT updated_at FROM workbooks WHERE id=$1`, id).Scan(&u)
	}); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestBudgetStampsUpdatedAtOnlyOnChange(t *testing.T) {
	e := newTelEnv(t)
	const wb = "wb-alpha-1"
	before := e.updatedAt(t, wb)
	if code, body := e.alice("PUT", "/api/workbooks/"+wb+"/budget", `{"max_usd":5}`); code != 200 {
		t.Fatal(code, body)
	}
	after := e.updatedAt(t, wb)
	if !after.After(before) {
		t.Fatalf("updated_at must advance when the cap changes: %v -> %v", before, after)
	}
	time.Sleep(20 * time.Millisecond)
	if code, _ := e.alice("PUT", "/api/workbooks/"+wb+"/budget", `{"max_usd":5.0}`); code != 200 {
		t.Fatal(code)
	}
	if again := e.updatedAt(t, wb); !again.Equal(after) {
		t.Fatalf("no-op assignment must not stamp updated_at: %v -> %v", after, again)
	}
	// NULL -> 0 is a change (the seed stores NULL).
	before = e.updatedAt(t, "wb-alpha-2")
	if code, body := e.alice("PUT", telBase+"/budget", `{}`); code != 200 || body != `{"budget_max_usd":0.0,"budget_spent_usd":0.0}` {
		t.Fatal(code, body)
	}
	if got := e.updatedAt(t, "wb-alpha-2"); !got.After(before) {
		t.Fatalf("NULL -> 0.0 must stamp updated_at: %v -> %v", before, got)
	}
}

func TestBudgetWritesNothingForForeignWorkbookOrViewer(t *testing.T) {
	e := newTelEnv(t)
	before := e.updatedAt(t, "wb-alpha-1")
	if code, body := e.call("bob", "ws-alpha", "PUT", "/api/workbooks/wb-alpha-1/budget", `{"max_usd":9}`); code != 403 {
		t.Errorf("viewer: %d %s", code, body)
	}
	if code, body := e.call("bob", "ws-beta", "PUT", "/api/workbooks/wb-alpha-1/budget", `{"max_usd":9}`); code != 404 {
		t.Errorf("foreign: %d %s", code, body)
	}
	if code, body := e.alice("PUT", "/api/workbooks/wb-alpha-1/budget", `{"max_usd":-1}`); code != 422 {
		t.Errorf("negative: %d %s", code, body)
	}
	if code, body := e.alice("PUT", "/api/workbooks/wb-alpha-1/budget", `{"max_usd":NaN}`); code != 422 {
		t.Errorf("NaN: %d %s", code, body)
	}
	if got := e.updatedAt(t, "wb-alpha-1"); !got.Equal(before) {
		t.Errorf("rejected writes changed updated_at")
	}
	_, body := e.get("alice", "ws-alpha", "/api/workbooks/wb-alpha-1/cost")
	if !strings.Contains(body, `"budget_max_usd":0.0`) {
		t.Errorf("cap changed: %s", body)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
