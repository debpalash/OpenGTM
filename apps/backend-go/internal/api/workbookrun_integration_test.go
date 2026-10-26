//go:build integration

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/billing"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ratelimit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/runplan"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/vendorcatalog"
)

const (
	runWsAlpha = "t24-ws-alpha"
	runWsBeta  = "t24-ws-beta"
)

var runWorkbooks = []string{"wb-enrich-1", "wb-enrich-big", "wb-enrich-v1", "wb-alpha-1", "wb-go"}

type runEnv struct {
	fnEnv
	owner *pgxpool.Pool
	users map[string]map[string]string
}

type runOpts struct {
	billing bool
	limiter *ratelimit.Limiter
	getenv  map[string]string
	manDir  string
}

func newRunEnv(t *testing.T, o runOpts) runEnv {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	pool := testpg.NewAppPool(t, 6)
	owner, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	if o.manDir != "" {
		prev := connectorsLookupEnv
		connectorsLookupEnv = func(k string) (string, bool) {
			if k == connectorManifestsEnv {
				return o.manDir, true
			}
			return "", false
		}
		t.Cleanup(func() { connectorsLookupEnv = prev })
	}
	// platform-billed research vendor (RESEARCH_VENDOR_COST on) through the production wiring
	runplan.Wire(runplan.WireDeps{AppSetting: func(context.Context, string, string) (string, error) { return "1", nil }})
	t.Cleanup(func() { runplan.Wire(runplan.WireDeps{}); vendorcatalog.SetResearchFlag(nil) })

	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Flags: config.Flags{PGLeadStore: true, GoRunRoutesEnabled: true},
		Config: config.APIConfig{BillingEnabled: o.billing}, Limiter: o.limiter,
		Getenv: func(k string) string { return o.getenv[k] }}
	registerWorkbookRun(r, d)
	e := runEnv{fnEnv: fnEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}, owner: owner}
	e.clean()
	t.Cleanup(e.clean)
	e.users = map[string]map[string]string{
		"alice":    e.user(t, runWsAlpha, "t24-alice", "admin", false),
		"erin":     e.user(t, runWsAlpha, "t24-erin", "editor", false),
		"bob":      e.user(t, runWsAlpha, "t24-bob", "viewer", false),
		"bob-beta": e.user(t, runWsBeta, "t24-bob-beta", "admin", false),
	}
	e.seedWorkbooks(t)
	return e
}

func (e runEnv) clean() {
	ctx := context.Background()
	ids := "'" + strings.Join(runWorkbooks, "','") + "'"
	for _, sql := range []string{
		`DELETE FROM jobs WHERE type='run_workbook' AND payload->>'workbook_id' IN (` + ids + `)`,
		`DELETE FROM workbook_views WHERE workbook_id IN (` + ids + `)`,
		`DELETE FROM workbook_rows WHERE workbook_id IN (` + ids + `)`,
		`DELETE FROM workbooks WHERE id IN (` + ids + `)`,
		`DELETE FROM credit_ledger_entries WHERE workspace_id IN ('` + runWsAlpha + `','` + runWsBeta + `')`,
		`DELETE FROM workspace_credits WHERE workspace_id IN ('` + runWsAlpha + `','` + runWsBeta + `')`,
	} {
		_, _ = e.owner.Exec(ctx, sql)
	}
}

type seedColumns struct {
	Columns   json.RawMessage `json:"columns"`
	V1Columns json.RawMessage `json:"v1_columns"`
	BigCols   json.RawMessage `json:"big_columns"`
}

// seed mirrors scripts/contract_seed.py _seed_enrichment (the state the run fixtures were recorded against).
func (e runEnv) seedWorkbooks(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "runplan", "testdata", "seed_columns.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s seedColumns
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.owner.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	wb := func(id, src, filter string, cols []byte) {
		exec(`INSERT INTO workbooks (id, name, description, status, workspace_id, source_type, source_config, filter_criteria, columns_config,
			total_rows, completed_rows, sync_to_leads, budget_spent_usd, created_at, updated_at)
			VALUES ($1,$1,'','idle',$2,$3,'{}'::json,$4::json,$5::json,0,0,false,0,'2026-08-29 10:00:00','2026-08-29 10:00:00')`,
			id, runWsAlpha, src, filter, string(cols))
	}
	wb("wb-enrich-1", "manual", "{}", s.Columns)
	wb("wb-enrich-big", "manual", "{}", s.BigCols)
	wb("wb-enrich-v1", "leads_filter", `{"city":"Berlin"}`, s.V1Columns)
	wb("wb-alpha-1", "manual", "{}", []byte(`[{"id":"company","name":"Company","type":"lead_field","lead_field":"company"}]`))
	wb("wb-go", "manual", "{}", []byte(`[{"id":"mob","name":"Mobile","type":"enrichment","provider":"prospeo_mobile","target_field":"mobile_phone"}]`))
	row := func(id int, wbID string, pos int, data, enr string) {
		exec(`INSERT INTO workbook_rows (id, workbook_id, "position", data, enrichments, workspace_id, created_at, updated_at)
			VALUES ($1,$2,$3,$4::json,$5::json,$6,'2026-08-29 10:00:00','2026-08-29 10:00:00')`, id, wbID, pos, data, enr, runWsAlpha)
	}
	sites := []string{"https://globex.example.com", "https://initech.example.com", "", "https://umbrella.example.com", "https://hooli.example.com", "https://stark.example.com"}
	companies := []string{"Globex", "Initech", "Soylent", "Umbrella", "Hooli", "Stark Industries"}
	for k := 0; k < 6; k++ {
		email := fmt.Sprintf("hello@%s.example.com", strings.ToLower(strings.Fields(companies[k])[0]))
		if k%2 == 1 {
			email = ""
		}
		data, _ := json.Marshal(map[string]any{"company": companies[k], "website": sites[k], "email": email})
		enr := fmt.Sprintf(`{"calc_len":{"value":"%d","status":"complete","provider":"formula","error":null}}`, len(companies[k]))
		row(101+k, "wb-enrich-1", k, string(data), enr)
	}
	for k := 0; k < 250; k++ {
		row(1001+k, "wb-enrich-big", k, fmt.Sprintf(`{"company":"Bulk %03d"}`, k+1), `{}`)
	}
	row(5001, "wb-go", 0, `{"company":"A"}`, `{}`)
	exec(`INSERT INTO workbook_views (id, workspace_id, workbook_id, name, config)
		VALUES ('view-enrich-1',$1,'wb-enrich-1','Has website',$2::json)`, runWsAlpha,
		`{"filters":[{"column":"website","op":"not_empty","value":null}],"hidden_columns":[]}`)
}

func (e runEnv) credit(t *testing.T, usd float64) {
	t.Helper()
	ctx := context.Background()
	if err := tenancy.InWorkspace(ctx, e.pool, runWsAlpha, func(tx pgx.Tx) error {
		_, err := billing.Credit(ctx, tx, runWsAlpha, usd, "seed", "t24-seed")
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (e runEnv) balance(t *testing.T) float64 {
	t.Helper()
	ctx := context.Background()
	var b float64
	if err := tenancy.InWorkspace(ctx, e.pool, runWsAlpha, func(tx pgx.Tx) (err error) {
		b, err = billing.Balance(ctx, tx, runWsAlpha)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return b
}

func (e runEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e runEnv) ledgerRows(t *testing.T) int {
	return e.count(t, `SELECT count(*) FROM credit_ledger_entries WHERE workspace_id=$1`, runWsAlpha)
}

func (e runEnv) jobs(t *testing.T, wb string) int {
	return e.count(t, `SELECT count(*) FROM jobs WHERE type='run_workbook' AND payload->>'workbook_id'=$1`, wb)
}

func (e runEnv) status(t *testing.T, wb string) string {
	t.Helper()
	var s string
	if err := e.owner.QueryRow(context.Background(), `SELECT status FROM workbooks WHERE id=$1`, wb).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (e runEnv) post(wb, body string, hdr map[string]string) (*httptest.ResponseRecorder, contract.Resp) {
	return e.do("POST", "/api/workbooks/"+wb+"/run", "application/json", body, hdr)
}

var runShape = []contract.Option{contract.IgnoreShapeOf("$.run_id", "$.job_id")}

func headersFor(e runEnv, name string) map[string]string {
	switch name {
	case "run_no_token":
		return nil
	case "run_viewer", "run_billing_viewer":
		return e.users["bob"]
	case "run_foreign":
		return e.users["bob-beta"]
	case "run_happy_editor":
		return e.users["erin"]
	}
	return e.users["alice"]
}

func replay(t *testing.T, e runEnv, name string) {
	t.Helper()
	fx := contract.Load(t, "run/"+name)
	body := string(fx.Request.Body)
	if body == "null" || body == "" {
		body = ""
	}
	_, got := e.do(fx.Request.Method, fx.Request.Path, "application/json", body, headersFor(e, name))
	contract.AssertEqual(t, got, fx.Response, runShape...)
}

// TestRunMatchesFixtures replays every FastAPI-recorded non-billing POST /run
// fixture (billing off) against the Go route.
func TestRunMatchesFixtures(t *testing.T) {
	e := newRunEnv(t, runOpts{})
	files, err := filepath.Glob("../../testdata/contract/run/run_*.json")
	if err != nil || len(files) < 30 {
		t.Fatalf("fixtures: %d %v", len(files), err)
	}
	n := 0
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		if strings.HasPrefix(name, "run_billing_") || name == "run_v1_legacy" { // billing chain below; v1 legacy needs the lead plane
			continue
		}
		n++
		t.Run(name, func(t *testing.T) { replay(t, e, name) })
	}
	if n < 25 {
		t.Fatalf("only %d fixtures replayed", n)
	}
	if e.ledgerRows(t) != 0 {
		t.Fatal("billing off must not write ledger rows")
	}
}

// TestRunBillingFixturesChain replays the billing profile in recorded order:
// balance 10.0 -> 9.7 -> 9.65 -> 4.65 and two exact 402s that write nothing.
func TestRunBillingFixturesChain(t *testing.T) {
	e := newRunEnv(t, runOpts{billing: true})
	e.credit(t, 10)
	for _, name := range []string{"run_billing_402", "run_billing_ok_debit", "run_billing_byok_no_debit", "run_billing_cell_scope_counts",
		"run_billing_viewer", "run_billing_skipped_no_debit", "run_billing_circular_no_debit", "run_billing_partial_rows", "run_billing_second_402"} {
		t.Run(name, func(t *testing.T) { replay(t, e, name) })
	}
	if got := e.balance(t); got != 4.65 {
		t.Fatalf("balance %v want 4.65", got)
	}
	if n := e.ledgerRows(t); n != 4 { // seed credit + three debits
		t.Fatalf("ledger rows %d want 4", n)
	}
}

// TestRunDebitAndEnqueueAtomic: an enqueue failure rolls the debit, the
// status flip and the supersede back (Python loses the credits here).
func TestRunDebitAndEnqueueAtomic(t *testing.T) {
	e := newRunEnv(t, runOpts{billing: true})
	e.credit(t, 10)
	prev := runEnqueue
	runEnqueue = func(context.Context, pgx.Tx, string, string, json.RawMessage) (int64, error) {
		return 0, errors.New("queue down")
	}
	t.Cleanup(func() { runEnqueue = prev })
	rec, _ := e.post("wb-enrich-1", `{"column_ids":["plat_x"]}`, e.users["alice"])
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "queue down") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if b := e.balance(t); b != 10 {
		t.Fatalf("debit survived the failed enqueue: balance %v", b)
	}
	if n := e.ledgerRows(t); n != 1 {
		t.Fatalf("ledger rows %d want 1 (the seed credit)", n)
	}
	if s := e.status(t, "wb-enrich-1"); s != "idle" {
		t.Fatalf("status %q: status update must roll back with the enqueue", s)
	}
	if e.jobs(t, "wb-enrich-1") != 0 {
		t.Fatal("no job may exist")
	}
}

func TestRunInsufficientCredits402NothingWritten(t *testing.T) {
	e := newRunEnv(t, runOpts{billing: true})
	e.credit(t, 10)
	_, got := e.post("wb-enrich-big", `{"column_ids":["plat_big"]}`, e.users["alice"])
	fx := contract.Load(t, "run/run_billing_402")
	contract.AssertEqual(t, got, fx.Response)
	// the recorded body spells the floats like Python (10.0, 12.5)
	if !strings.Contains(string(got.Body), `"balance_usd":10.0`) || !strings.Contains(string(got.Body), `"required_usd":12.5`) {
		t.Fatalf("float spelling: %s", got.Body)
	}
	if e.ledgerRows(t) != 1 || e.balance(t) != 10 || e.jobs(t, "wb-enrich-big") != 0 || e.status(t, "wb-enrich-big") != "idle" {
		t.Fatal("a 402 must write nothing")
	}
}

// TestRunReplayedRunIDChargesOnce: the same run_id twice debits once; the
// second call is the replay and still answers 200.
func TestRunReplayedRunIDChargesOnce(t *testing.T) {
	e := newRunEnv(t, runOpts{billing: true})
	e.credit(t, 10)
	prev := runNewID
	runNewID = func() string { return "fixedrunid0001" }
	t.Cleanup(func() { runNewID = prev })
	for i := 0; i < 2; i++ {
		rec, got := e.post("wb-enrich-1", `{"column_ids":["plat_x"]}`, e.users["alice"])
		if rec.Code != 200 {
			t.Fatalf("call %d: %d %s", i, rec.Code, got.Body)
		}
	}
	if b := e.balance(t); b != 9.7 {
		t.Fatalf("balance %v want 9.7 (charged once)", b)
	}
	if n := e.count(t, `SELECT count(*) FROM credit_ledger_entries WHERE workspace_id=$1 AND run_id='fixedrunid0001'`, runWsAlpha); n != 1 {
		t.Fatalf("debit rows for the run: %d", n)
	}
}

func TestRunBillingOffNeverTouchesLedger(t *testing.T) {
	e := newRunEnv(t, runOpts{})
	rec, _ := e.post("wb-enrich-1", `{"column_ids":["plat_x"]}`, e.users["alice"])
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if e.ledgerRows(t) != 0 || e.count(t, `SELECT count(*) FROM workspace_credits WHERE workspace_id=$1`, runWsAlpha) != 0 {
		t.Fatal("billing off: ledger and balance rows must not exist")
	}
	if e.jobs(t, "wb-enrich-1") != 1 {
		t.Fatal("run still enqueues")
	}
}

func TestRunRateLimit20PerMinute(t *testing.T) {
	e := newRunEnv(t, runOpts{limiter: ratelimit.New(nil)})
	for i := 1; i <= 20; i++ {
		if rec, _ := e.post("wb-alpha-1", `{"column_ids":["nope"]}`, e.users["alice"]); rec.Code != 200 {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	rec, _ := e.post("wb-alpha-1", `{"column_ids":["nope"]}`, e.users["alice"])
	if rec.Code != 429 || !strings.Contains(rec.Body.String(), "Rate limit exceeded: 20 per 1 minute") {
		t.Fatalf("21st: %d %s", rec.Code, rec.Body)
	}
	// the limiter sits after authentication: an anonymous call is 401, not counted
	if rec, _ := e.post("wb-alpha-1", `{}`, nil); rec.Code != 401 {
		t.Fatalf("anonymous: %d", rec.Code)
	}
}

func TestRunCycle422(t *testing.T) {
	e := newRunEnv(t, runOpts{})
	replay(t, e, "run_circular")
	if e.jobs(t, "wb-enrich-1") != 0 || e.status(t, "wb-enrich-1") != "idle" {
		t.Fatal("a rejected run writes nothing")
	}
}

func TestRunForeignWorkbook404(t *testing.T) {
	e := newRunEnv(t, runOpts{})
	replay(t, e, "run_foreign")
	replay(t, e, "run_unknown")
	if e.jobs(t, "wb-enrich-1") != 0 {
		t.Fatal("a foreign run writes nothing")
	}
}

// TestRunExecutorFollowsCohort: the job row's executor comes from the plan.
func TestRunExecutorFollowsCohort(t *testing.T) {
	dir, _ := filepath.Abs(filepath.Join("..", "runplan", "testdata", "manifests"))
	env := map[string]string{}
	e := newRunEnv(t, runOpts{getenv: env, manDir: dir}) // one env: each env holds the suite-wide DB lock
	run := func(vars map[string]string) (executor, payload string) {
		for k := range env {
			delete(env, k)
		}
		for k, v := range vars {
			env[k] = v
		}
		_, _ = e.owner.Exec(context.Background(), `DELETE FROM jobs WHERE type='run_workbook' AND payload->>'workbook_id'='wb-go'`)
		rec, _ := e.post("wb-go", `{"column_ids":["mob"]}`, e.users["alice"])
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if err := e.owner.QueryRow(context.Background(),
			`SELECT executor, payload::text FROM jobs WHERE type='run_workbook' AND payload->>'workbook_id'='wb-go'`).Scan(&executor, &payload); err != nil {
			t.Fatal(err)
		}
		return
	}
	ex, p := run(map[string]string{"GO_EXECUTOR_ENABLED": "1", "GO_EXECUTOR_WORKSPACES": runWsAlpha})
	var job struct {
		WorkspaceID string          `json:"workspace_id"`
		Go          json.RawMessage `json:"go"`
	}
	if err := json.Unmarshal([]byte(p), &job); err != nil || ex != "go" || len(job.Go) == 0 || job.WorkspaceID != runWsAlpha {
		t.Fatalf("cohort member: executor %s payload %s (%v)", ex, p, err)
	}
	if ex, p := run(map[string]string{"GO_EXECUTOR_ENABLED": "1", "GO_EXECUTOR_WORKSPACES": "someone-else"}); ex != "python" || strings.Contains(p, `"go"`) {
		t.Fatalf("outside cohort: executor %s payload %s", ex, p)
	}
	if ex, _ := run(nil); ex != "python" {
		t.Fatalf("flag off: executor %s", ex)
	}
}

// TestRunSupersedesInflightRun: a new run cancels the workbook's pending run job.
func TestRunSupersedesInflightRun(t *testing.T) {
	e := newRunEnv(t, runOpts{})
	for i := 0; i < 2; i++ {
		if rec, _ := e.post("wb-enrich-1", `{"column_ids":["calc_len"]}`, e.users["alice"]); rec.Code != 200 {
			t.Fatal(rec.Body)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM jobs WHERE type='run_workbook' AND payload->>'workbook_id'='wb-enrich-1' AND status='cancelled' AND error='superseded by a newer run'`); n != 1 {
		t.Fatalf("superseded jobs: %d", n)
	}
	if s := e.status(t, "wb-enrich-1"); s != "running" {
		t.Fatalf("status %q", s)
	}
}

// TestRunBodyLimit: the request body is capped (413), as on the other Go write routes.
func TestRunBodyLimit(t *testing.T) {
	e := newRunEnv(t, runOpts{})
	big := `{"column_ids":["calc_len"],"search":"` + strings.Repeat("a", maxRunBody+1) + `"}`
	rec, _ := e.post("wb-enrich-1", big, e.users["alice"])
	if rec.Code != 413 || !strings.Contains(rec.Body.String(), "Request body too large") {
		t.Fatalf("%d %.100s", rec.Code, rec.Body)
	}
	if e.jobs(t, "wb-enrich-1") != 0 {
		t.Fatal("rejected body writes nothing")
	}
}
