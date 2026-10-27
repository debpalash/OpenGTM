//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ratelimit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// cellEnv mounts only the cell run route; python is a stand-in for the FastAPI
// process that must never be called (the output columns once were proxied to it).
type cellEnv struct {
	leadsEnv
	python     *httptest.Server
	pyHits     atomic.Int32
	pyStatus   int
	pyBody     string
	pyLastBody atomic.Value // string
	pyLastAuth atomic.Value // string
	runner     *cellFakeRunner
	pyURL      string       // PYTHON_API_URL override (empty = the stand-in)
	pyLastXFF  atomic.Value // string
}

type cellFakeRunner struct {
	mu    sync.Mutex
	calls int
	block chan struct{} // when non-nil, Run waits for it or the context
}

func (f *cellFakeRunner) Run(ctx context.Context, _, _ string, _ providers.Input, _ time.Duration) (providers.Output, error) {
	f.mu.Lock()
	f.calls++
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return providers.Output{}, ctx.Err()
		}
	}
	o := providers.NewObj()
	o.Set("founded_year", "1999")
	return providers.Output{Confidence: 0.7, Extra: o}, nil
}

func newCellEnv(t *testing.T) *cellEnv { return newCellEnvURL(t, "") }

func newCellEnvURL(t *testing.T, pyURL string) *cellEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	ce := &cellEnv{pyStatus: 200, pyBody: `{"status":"complete","value":"pushed","provider":"webhook","error":null,"skipped":false,"forced":true,"research":null}`,
		runner: &cellFakeRunner{}, pyURL: pyURL}
	ce.python = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ce.pyHits.Add(1)
		b, _ := io.ReadAll(r.Body)
		ce.pyLastBody.Store(string(b))
		ce.pyLastXFF.Store(r.Header.Get("X-Forwarded-For") + "|" + r.Header.Get("X-Real-Ip"))
		ce.pyLastAuth.Store(r.Header.Get("Authorization") + "|" + r.Header.Get("X-Workspace-Id") + "|" + r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-From-Python", "yes")
		w.WriteHeader(ce.pyStatus)
		_, _ = io.WriteString(w, ce.pyBody)
	}))
	t.Cleanup(ce.python.Close)
	prev := newCellRunner
	newCellRunner = func(Deps) workbook.ProviderRunner { return ce.runner }
	t.Cleanup(func() { newCellRunner = prev })

	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Log: quietLog(),
		Flags:   config.Flags{PGLeadStore: true, GoRunRoutesEnabled: true},
		Limiter: ratelimit.New(nil),
		Getenv: func(k string) string {
			if k == "PYTHON_API_URL" {
				if ce.pyURL != "" {
					return ce.pyURL
				}
				return ce.python.URL
			}
			return ""
		}}
	registerWorkbookCell(r, d)
	ce.leadsEnv = leadsEnv{mcpEnv{env: env0(r, st), pool: pool}}
	ce.purge()
	ce.seedLeads(t)
	ce.seed(t, "erin", pythonHash, true, "user", false)
	ce.member(t, "ws-alpha", ce.mustUserByName(t, "erin").ID, "editor")
	ce.clean()
	t.Cleanup(ce.clean)
	t.Cleanup(ce.purge)
	ce.seedWorkbooks(t)
	return ce
}

func (e *cellEnv) clean() {
	for _, q := range []string{
		`DELETE FROM workbook_spend_attempts WHERE workspace_id IN ('ws-alpha','ws-beta')`,
		`DELETE FROM workbook_enrichments WHERE workspace_id IN ('ws-alpha','ws-beta')`,
		`DELETE FROM jobs WHERE workspace_id IN ('ws-alpha','ws-beta') AND type = 'run_workbook'`,
		`DELETE FROM workbook_rows WHERE workbook_id IN (SELECT id FROM workbooks WHERE workspace_id IN ('ws-alpha','ws-beta'))`,
		`DELETE FROM workbooks WHERE workspace_id IN ('ws-alpha','ws-beta')`,
	} {
		c, err := pgx.Connect(context.Background(), os.Getenv("TEST_DATABASE_URL"))
		if err != nil {
			return
		}
		_, _ = c.Exec(context.Background(), q)
		_ = c.Close(context.Background())
	}
}

// seedWorkbooks mirrors contract_seed.py _seed_enrichment (wb-enrich-1 with six rows,
// wb-enrich-big, the v1 leads-filter wb-enrich-v1) plus a paid-provider workbook.
func (e *cellEnv) seedWorkbooks(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "runplan", "testdata", "seed_columns.json"))
	if err != nil {
		t.Fatal(err)
	}
	var seed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &seed); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO workbooks (id, name, description, status, workspace_id, source_type, source_config, filter_criteria,
		columns_config, total_rows, completed_rows, sync_to_leads, budget_max_usd, budget_spent_usd)
		VALUES ($1,$1,'', 'idle', $2, $3, '{}'::json, $4::json, $5::json, 0, 0, false, $6, 0)`
	ownerExec(t, ins, "wb-enrich-1", "ws-alpha", "manual", "{}", string(seed["columns"]), nil)
	ownerExec(t, ins, "wb-enrich-big", "ws-alpha", "manual", "{}", string(seed["big_columns"]), nil)
	ownerExec(t, ins, "wb-enrich-v1", "ws-alpha", "leads_filter", `{"city":"Berlin"}`, string(seed["v1_columns"]), nil)
	ownerExec(t, ins, "wb-paid", "ws-alpha", "manual", "{}",
		`[{"id":"c1","type":"waterfall","name":"C1","target_field":"founded_year","waterfall":["hunter_io"]}]`, 10.0)
	sites := []string{"https://globex.example.com", "https://initech.example.com", "", "https://umbrella.example.com", "https://hooli.example.com", "https://stark.example.com"}
	companies := []string{"Globex", "Initech", "Soylent", "Umbrella", "Hooli", "Stark Industries"}
	for k := 0; k < 6; k++ {
		email := fmt.Sprintf("hello@%s.example.com", strings.ToLower(strings.Fields(companies[k])[0]))
		if k%2 == 1 {
			email = ""
		}
		data, _ := json.Marshal(map[string]any{"company": companies[k], "website": sites[k], "email": email})
		enr := map[string]any{"calc_len": map[string]any{"value": fmt.Sprint(len(companies[k])), "status": "complete", "provider": "formula", "error": nil}}
		if k == 1 {
			enr["out_hook"] = map[string]any{"value": "pushed", "status": "complete", "provider": "webhook", "error": nil}
		}
		eb, _ := json.Marshal(enr)
		e.row(t, 101+k, "wb-enrich-1", k, string(data), string(eb))
	}
	e.row(t, 1001, "wb-enrich-big", 0, `{"company":"Bulk 001"}`, `{}`)
	e.row(t, 2001, "wb-paid", 0, `{"company":"Paid Co","website":"paid.example.com"}`, `{}`)
}

func (e *cellEnv) row(t *testing.T, id int, wb string, pos int, data, enr string) {
	t.Helper()
	ownerExec(t, `INSERT INTO workbook_rows (id, workbook_id, "position", data, enrichments, workspace_id) VALUES ($1,$2,$3,$4::json,$5::json,'ws-alpha')`,
		id, wb, pos, data, enr)
	ownerExec(t, `SELECT setval(pg_get_serial_sequence('workbook_rows','id'), GREATEST((SELECT max(id) FROM workbook_rows), 1))`)
}

func cellActor(name string) (who, ws string) {
	switch name {
	case "cell_no_token":
		return "", ""
	case "cell_editor":
		return "erin", "ws-alpha"
	case "cell_viewer":
		return "bob", "ws-alpha"
	case "cell_foreign":
		return "bob", "ws-beta"
	}
	return "alice", "ws-alpha"
}

func (e *cellEnv) post(who, ws, path, body string) (*httptest.ResponseRecorder, contract.Resp) {
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
	return e.do("POST", path, ct, body, hdr)
}

func TestCellRunMatchesFixtures(t *testing.T) {
	e := newCellEnv(t)
	names := contractFixtureNames(t, "cellrun")
	if len(names) != 17 {
		t.Fatalf("expected 17 cellrun fixtures, found %d", len(names))
	}
	for _, full := range names {
		name := strings.TrimPrefix(full, "cellrun/")
		fx := contract.Load(t, full)
		who, ws := cellActor(name)
		body := ""
		if len(fx.Request.Body) > 0 && string(fx.Request.Body) != "null" {
			body = string(fx.Request.Body)
		}
		t.Run(name, func(t *testing.T) {
			_, g := e.post(who, ws, fx.Request.Path, body)
			contract.AssertEqual(t, g, fx.Response)
		})
	}
	if e.pyHits.Load() != 0 {
		t.Fatalf("a fixture case reached the python api %d times", e.pyHits.Load())
	}
}

func (e *cellEnv) overlayStatus(t *testing.T, wb string, row int, col string) string {
	t.Helper()
	var s *string
	c, err := pgx.Connect(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	if err := c.QueryRow(context.Background(), `SELECT enrichments::jsonb->$3->>'status' FROM workbook_rows WHERE workbook_id=$1 AND id=$2`, wb, row, col).Scan(&s); err != nil {
		t.Fatal(err)
	}
	if s == nil {
		return ""
	}
	return *s
}

func TestCellRunSkipsCompleteOutputWithoutForce(t *testing.T) {
	e := newCellEnv(t)
	rec, _ := e.post("alice", "ws-alpha", "/api/workbooks/wb-enrich-1/rows/102/cells/out_hook/run", `{}`)
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if got["skipped"] != true || got["status"] != "skipped" || got["value"] != "pushed" || got["forced"] != false {
		t.Fatalf("complete output cell without force: %v", got)
	}
	if e.pyHits.Load() != 0 {
		t.Fatal("a skipped output cell must not reach python")
	}
}

// Output columns run in this process (plan 15: the Python API proxy is gone). PYTHON_API_URL still names a
// stand-in here, which must never be called.
func TestCellRunOutputColumnsRunInProcess(t *testing.T) {
	e := newCellEnv(t)
	for _, c := range []struct{ row, body string }{{"102", `{"force": true}`}, {"101", `{}`}} {
		rec, _ := e.post("alice", "ws-alpha", "/api/workbooks/wb-enrich-1/rows/"+c.row+"/cells/out_hook/run", c.body)
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 || got["status"] == nil || rec.Header().Get("X-From-Python") != "" {
			t.Fatalf("row %s: %d %q", c.row, rec.Code, rec.Body.String())
		}
	}
	if e.pyHits.Load() != 0 {
		t.Fatalf("the python api was called %d times", e.pyHits.Load())
	}
}

func TestCellRunRateLimit120(t *testing.T) {
	e := newCellEnv(t)
	path := "/api/workbooks/wb-enrich-1/rows/101/cells/calc_len/run"
	for i := 1; i <= 120; i++ {
		if rec, _ := e.post("alice", "ws-alpha", path, `{}`); rec.Code != 200 {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	rec, _ := e.post("alice", "ws-alpha", path, `{}`)
	if rec.Code != 429 || !strings.Contains(rec.Body.String(), "Rate limit exceeded: 120 per 1 minute") {
		t.Fatalf("121st request: %d %s", rec.Code, rec.Body.String())
	}
	// Auth failures never consume the budget (slowapi runs inside the dependency chain).
	if rec, _ := e.post("", "", path, `{}`); rec.Code != 401 {
		t.Fatalf("unauthenticated: %d", rec.Code)
	}
}

// Only a legacy leads-filter workbook with no stored rows resolves row_id as a lead id.
func TestCellRunLegacyLeadRowResolution(t *testing.T) {
	e := newCellEnv(t)
	rec, _ := e.post("alice", "ws-alpha", "/api/workbooks/wb-enrich-v1/rows/2/cells/calc_len/run", `{}`)
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if got["status"] != "complete" || got["value"] != float64(6) || got["provider"] != "formula" {
		t.Fatalf("legacy lead row 2 (Globex): %v", got)
	}
	c, err := pgx.Connect(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	var n int
	if err := c.QueryRow(context.Background(), `SELECT count(*) FROM workbook_enrichments WHERE workbook_id='wb-enrich-v1' AND lead_id=2 AND column_id='calc_len' AND status='complete'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("overlay rows %d %v", n, err)
	}
	// Lead 3 is not in the Berlin filter, lead 9999 does not exist: both are 404 "Row not found".
	for _, id := range []string{"1", "9999"} {
		rec, _ := e.post("alice", "ws-alpha", "/api/workbooks/wb-enrich-v1/rows/"+id+"/cells/calc_len/run", `{}`)
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"Row not found"`) {
			t.Fatalf("lead %s: %d %s", id, rec.Code, rec.Body.String())
		}
	}
	// A v2 workbook never reinterprets a missing row as a lead id.
	if rec, _ := e.post("alice", "ws-alpha", "/api/workbooks/wb-enrich-1/rows/2/cells/calc_len/run", `{}`); rec.Code != 404 ||
		!strings.Contains(rec.Body.String(), `"Workbook row not found"`) {
		t.Fatalf("v2 workbook, lead id as row id: %d %s", rec.Code, rec.Body.String())
	}
}

// A row of another workbook (same workspace) or another workspace is never run.
func TestCellRunForeignRow404(t *testing.T) {
	e := newCellEnv(t)
	for _, c := range []struct{ who, ws, path, want string }{
		{"alice", "ws-alpha", "/api/workbooks/wb-enrich-1/rows/1001/cells/calc_len/run", "Workbook row not found"},
		{"alice", "ws-alpha", "/api/workbooks/wb-enrich-big/rows/101/cells/company/run", "Enrichment column not found"},
		{"bob", "ws-beta", "/api/workbooks/wb-enrich-1/rows/101/cells/calc_len/run", "Workbook not found"},
	} {
		rec, _ := e.post(c.who, c.ws, c.path, `{}`)
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: %d %s", c.path, rec.Code, rec.Body.String())
		}
	}
	if e.overlayStatus(t, "wb-enrich-big", 1001, "calc_len") != "" {
		t.Fatal("a foreign row was written")
	}
}

func TestCellRunRejectsBadBodies(t *testing.T) {
	e := newCellEnv(t)
	path := "/api/workbooks/wb-enrich-1/rows/101/cells/calc_len/run"
	for _, body := range []string{`{`, `[]`, `{"force":"maybe"}`, `{"force":null}`} {
		if rec, _ := e.post("alice", "ws-alpha", path, body); rec.Code != 422 {
			t.Errorf("body %s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	for _, body := range []string{`{"force":"yes"}`, `{"force":1}`, `{"force":false,"extra":1}`} {
		if rec, _ := e.post("alice", "ws-alpha", path, body); rec.Code != 200 {
			t.Errorf("body %s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	rec, _ := e.post("alice", "ws-alpha", path, `{"force":"on"}`)
	if !strings.Contains(rec.Body.String(), `"forced":true`) {
		t.Fatalf("forced not echoed: %s", rec.Body.String())
	}
}

// After the paid call is dispatched a client disconnect does not cancel the run: the cell is
// persisted and the attempt settles; only the answer (499) is dropped.
func TestCellRunCancelOnClientDisconnect(t *testing.T) {
	e := newCellEnv(t)
	e.runner.block = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/api/workbooks/wb-paid/rows/2001/cells/c1/run", strings.NewReader(`{}`)).WithContext(ctx)
	req.Header.Set("X-Workspace-Id", "ws-alpha")
	for k, v := range bearer("alice") {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { e.h.ServeHTTP(rec, req); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.runner.mu.Lock()
		n := e.runner.calls
		e.runner.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the provider call never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	close(e.runner.block)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return")
	}
	if rec.Code != 499 {
		t.Fatalf("status %d, want 499", rec.Code)
	}
	c, err := pgx.Connect(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	var st string
	if err := c.QueryRow(context.Background(), `SELECT status FROM workbook_spend_attempts WHERE workbook_id='wb-paid'`).Scan(&st); err != nil || st != "settled" {
		t.Fatalf("attempt %q %v", st, err)
	}
	if got := e.overlayStatus(t, "wb-paid", 2001, "c1"); got != "complete" {
		t.Fatalf("cell not persisted: %q", got)
	}
}

// The API-side runner resolves provider credentials per workspace through the settings
// resolver (so a keyed provider is configured), and without a secrets envelope fails closed.
func TestCellRunnerResolvesWorkspaceCredentials(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	envl, err := secrets.NewEnvelope(secrets.Config{MasterKey: "cell-run-test-key", AppEnv: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		if k == "HUNTER_API_KEY" {
			return "k-from-env"
		}
		return ""
	}
	with := newCellRunner(Deps{Pool: pool, Envelope: envl, Getenv: getenv, Log: quietLog()}).(interface {
		Configured(context.Context, string, string) (bool, error)
	})
	if ok, err := with.Configured(context.Background(), "ws-alpha", "hunter_io"); err != nil || !ok {
		t.Fatalf("keyed provider with a resolvable key: %v %v", ok, err)
	}
	without := newCellRunner(Deps{Pool: pool, Getenv: getenv, Log: quietLog()}).(interface {
		Configured(context.Context, string, string) (bool, error)
	})
	if ok, _ := without.Configured(context.Background(), "ws-alpha", "hunter_io"); ok {
		t.Fatal("no envelope must not resolve credentials")
	}
}
