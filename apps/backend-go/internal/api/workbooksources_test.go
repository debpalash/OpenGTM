//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ratelimit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type srcEnv struct {
	mcpEnv
	router *httpx.Router
}

const srcColsEnrich1 = `[
 {"id":"calc_len","name":"Len","type":"formula","width":160,"formula":"len({company})"},
 {"id":"src_icp","name":"ICP Source","type":"source","width":200,"icp":{"description":"B2B SaaS","industry":"software"},"channels":{},"target_rows":0}
]`

// newSrcEnv seeds the plan-06 contract world: alice (admin, owner of ws-alpha), erin (editor), bob (member of
// ws-alpha, owner of ws-beta) and workbooks wb-enrich-1 / -big / -live in ws-alpha.
func newSrcEnv(t *testing.T, getenv func(string) string) srcEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Flags: config.Flags{PGLeadStore: true},
		Limiter: ratelimit.New(nil), Getenv: getenv}
	registerWorkbookSources(r, d)
	e := srcEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}, r}
	e.purge()
	t.Cleanup(e.purge)
	e.seed(t, "alice", pythonHash, true, "admin", true)
	e.seed(t, "bob", pythonHash, true, "user", false)
	e.seed(t, "erin", pythonHash, true, "user", false)
	alice, bob, erin := e.mustUserByName(t, "alice"), e.mustUserByName(t, "bob"), e.mustUserByName(t, "erin")
	e.workspace(t, "ws-alpha", alice.ID)
	e.workspace(t, "ws-beta", bob.ID)
	e.member(t, "ws-alpha", alice.ID, "owner")
	e.member(t, "ws-alpha", bob.ID, "member")
	e.member(t, "ws-alpha", erin.ID, "editor")
	e.member(t, "ws-beta", bob.ID, "owner")
	for _, w := range []struct{ id, cols string }{{"wb-enrich-1", srcColsEnrich1}, {"wb-enrich-big", `[]`}, {"wb-enrich-live", srcColsEnrich1}} {
		w := w
		err := tenancy.InWorkspace(context.Background(), pool, "ws-alpha", func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `INSERT INTO workbooks (id,name,workspace_id,columns_config,created_at,updated_at)
				VALUES ($1,$1,'ws-alpha',$2::json,'2026-01-01 00:00:00','2026-01-01 00:00:00')`, w.id, w.cols)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e srcEnv) purge() {
	ctx := context.Background()
	_, _ = e.pool.Exec(ctx, `DELETE FROM jobs WHERE type='source_workbook'`)
	for _, ws := range []string{"ws-alpha", "ws-beta"} {
		_ = tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM workbooks`)
			return err
		})
	}
}

func (e srcEnv) call(method, path, who, ws, body string) (int, contract.Resp) {
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
	rec, resp := e.do(method, path, ct, body, hdr)
	return rec.Code, resp
}

func (e srcEnv) columns(t *testing.T, wb string) []map[string]any {
	t.Helper()
	var raw string
	err := tenancy.InWorkspace(context.Background(), e.pool, "ws-alpha", func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT columns_config::text FROM workbooks WHERE id=$1`, wb).Scan(&raw)
	})
	if err != nil {
		t.Fatal(err)
	}
	var cols []map[string]any
	if err := json.Unmarshal([]byte(raw), &cols); err != nil {
		t.Fatal(err)
	}
	return cols
}

func (e srcEnv) jobs(t *testing.T) (n int) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE type='source_workbook'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// srcCase maps a recorded fixture to the caller it was recorded with (headers are redacted in fixtures).
type srcCase struct{ name, who, ws string }

func (e srcEnv) replay(t *testing.T, cases []srcCase) {
	t.Helper()
	for _, c := range cases {
		fx := contract.Load(t, "sources/"+c.name)
		body := ""
		if len(fx.Request.Body) > 0 && string(fx.Request.Body) != "null" {
			body = string(fx.Request.Body)
		}
		_, got := e.call(fx.Request.Method, fx.Request.Path, c.who, c.ws, body)
		t.Run(c.name, func(t *testing.T) {
			contract.AssertEqual(t, got, fx.Response, contract.IgnoreShapeOf("$.column.id"))
		})
	}
}

// TestAddSourceMatchesFixtures replays testdata/contract/sources/sources_add_*.json (recorded from FastAPI).
func TestAddSourceMatchesFixtures(t *testing.T) {
	e := newSrcEnv(t, nil)
	e.replay(t, []srcCase{
		{"sources_add_icp", "alice", "ws-alpha"},
		{"sources_add_defaults", "alice", "ws-alpha"},
		{"sources_add_crm_source", "alice", "ws-alpha"},
		{"sources_add_people_search_disabled", "alice", "ws-alpha"},
		{"sources_add_bad_body", "alice", "ws-alpha"},
		{"sources_add_viewer", "bob", "ws-alpha"},
		{"sources_add_foreign", "bob", "ws-beta"},
		{"sources_add_unknown_wb", "alice", "ws-alpha"},
		{"sources_add_no_token", "", ""},
	})
	cols := e.columns(t, "wb-enrich-1")
	if len(cols) != 3 || cols[2]["name"] != "Contract ICP" || !strings.HasPrefix(cols[2]["id"].(string), "src_") {
		t.Fatalf("stored columns = %v", cols)
	}
}

func TestPeopleSearchDisabled403(t *testing.T) {
	e := newSrcEnv(t, nil)
	body := `{"kind":"people_search","companies":["Acme"],"titles":["CTO"]}`
	code, resp := e.call("POST", "/api/workbooks/wb-enrich-1/sources", "alice", "ws-alpha", body)
	if code != 403 || string(resp.Body) != `{"detail":"people_search source is disabled (PEOPLE_SEARCH_SOURCE_ENABLED)"}` {
		t.Fatalf("got %d %s", code, resp.Body)
	}
	if n := len(e.columns(t, "wb-enrich-1")); n != 2 {
		t.Fatalf("a disabled people_search source stored a column (%d columns)", n)
	}
	// The workbook lookup precedes the flag check: an unknown workbook is 404 even with the flag off.
	if code, _ := e.call("POST", "/api/workbooks/nope/sources", "alice", "ws-alpha", body); code != 404 {
		t.Fatalf("unknown workbook = %d", code)
	}
}

func TestPeopleSearchEnabled(t *testing.T) {
	e := newSrcEnv(t, func(k string) string {
		if k == "PEOPLE_SEARCH_SOURCE_ENABLED" {
			return "true"
		}
		return ""
	})
	code, resp := e.call("POST", "/api/workbooks/wb-enrich-1/sources", "alice", "ws-alpha",
		`{"kind":"people_search","name":"People","companies":[" Acme ","",  "stripe.com"],"titles":["CTO"],"geo":" Berlin ","max_per_company":99,"max_searches":"7"}`)
	if code != 200 {
		t.Fatalf("got %d %s", code, resp.Body)
	}
	var got struct{ Column map[string]any }
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	id, _ := got.Column["id"].(string)
	want := fmt.Sprintf(`{"column":{"id":%q,"name":"People","type":"source","kind":"people_search","companies":["Acme","stripe.com"],"from_column":"","titles":["CTO"],"seniority":"","geo":"Berlin","max_per_company":25,"max_searches":7}}`, id)
	if string(resp.Body) != want { // key order is part of the contract
		t.Fatalf("body\n got %s\nwant %s", resp.Body, want)
	}
	// No companies and no from_column: Python's ValueError text as a string 422 detail.
	code, resp = e.call("POST", "/api/workbooks/wb-enrich-1/sources", "alice", "ws-alpha", `{"kind":"people_search"}`)
	if code != 422 || string(resp.Body) != `{"detail":"people_search requires 'companies' or 'from_column'"}` {
		t.Fatalf("got %d %s", code, resp.Body)
	}
}

func TestAddSourceBodyShapes(t *testing.T) {
	e := newSrcEnv(t, nil)
	for _, c := range []struct {
		name, body, want string
	}{
		{"empty", ``, `{"detail":[{"type":"missing","loc":["body"],"msg":"Field required","input":null}]}`},
		{"array", `[]`, `{"detail":[{"type":"model_attributes_type","loc":["body"],"msg":"Input should be a valid dictionary or object to extract fields from","input":[]}]}`},
		{"name_int", `{"name":5}`, `{"detail":[{"type":"string_type","loc":["body","name"],"msg":"Input should be a valid string","input":5}]}`},
		{"companies_item", `{"companies":["a",2]}`, `{"detail":[{"type":"string_type","loc":["body","companies",1],"msg":"Input should be a valid string","input":2}]}`},
		{"icp_list", `{"icp":[]}`, `{"detail":[{"type":"dict_type","loc":["body","icp"],"msg":"Input should be a valid dictionary","input":[]}]}`},
		{"two_errors_in_field_order", `{"max_searches":"x","target_rows":1.5}`, `{"detail":[{"type":"int_from_float","loc":["body","target_rows"],"msg":"Input should be a valid integer, got a number with a fractional part","input":1.5},{"type":"int_parsing","loc":["body","max_searches"],"msg":"Input should be a valid integer, unable to parse string as an integer","input":"x"}]}`},
	} {
		code, resp := e.call("POST", "/api/workbooks/wb-enrich-1/sources", "alice", "ws-alpha", c.body)
		if code != 422 || string(resp.Body) != c.want {
			t.Errorf("%s: got %d %s\nwant %s", c.name, code, resp.Body, c.want)
		}
	}
	// Lax coercion: a numeric string target_rows is accepted.
	if code, resp := e.call("POST", "/api/workbooks/wb-enrich-1/sources", "alice", "ws-alpha", `{"target_rows":"12"}`); code != 200 || !strings.Contains(string(resp.Body), `"target_rows":12`) {
		t.Errorf("lax int: %d %s", code, resp.Body)
	}
}

// TestAddSourceConcurrentAppendsBoth: the workbook row lock serialises two adds; neither append is lost.
func TestAddSourceConcurrentAppendsBoth(t *testing.T) {
	e := newSrcEnv(t, nil)
	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = e.call("POST", "/api/workbooks/wb-enrich-big/sources", "alice", "ws-alpha", fmt.Sprintf(`{"name":"S%d"}`, i))
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("request %d = %d", i, c)
		}
	}
	cols := e.columns(t, "wb-enrich-big")
	seen := map[string]bool{}
	for _, c := range cols {
		seen[c["name"].(string)] = true
	}
	if len(cols) != n || len(seen) != n {
		t.Fatalf("lost an append: %d columns, names %v", len(cols), seen)
	}
}

func TestSourceRunMatchesFixtures(t *testing.T) {
	e := newSrcEnv(t, nil)
	e.replay(t, []srcCase{
		{"sources_run", "alice", "ws-alpha"},
		{"sources_run_editor", "erin", "ws-alpha"},
		{"sources_run_not_source", "alice", "ws-alpha"},
		{"sources_run_missing_col", "alice", "ws-alpha"},
		{"sources_run_viewer", "bob", "ws-alpha"},
		{"sources_run_foreign", "bob", "ws-beta"},
		{"sources_run_unknown_wb", "alice", "ws-alpha"},
		{"sources_run_no_token", "", ""},
	})
	if n := e.jobs(t); n != 2 {
		t.Fatalf("jobs enqueued = %d, want 2 (only the two accepted runs)", n)
	}
}

func TestSourceRunEnqueuesPythonExecutor(t *testing.T) {
	e := newSrcEnv(t, nil)
	if code, resp := e.call("POST", "/api/workbooks/wb-enrich-1/sources/src_icp/run", "alice", "ws-alpha", ""); code != 200 {
		t.Fatalf("got %d %s", code, resp.Body)
	}
	var typ, executor, status, ws, payload string
	var fk *string
	err := e.pool.QueryRow(context.Background(),
		`SELECT type, executor, status, workspace_id, fire_key, payload::text FROM jobs WHERE type='source_workbook'`).
		Scan(&typ, &executor, &status, &ws, &fk, &payload)
	if err != nil {
		t.Fatal(err)
	}
	if executor != "python" || status != "pending" || ws != "ws-alpha" || fk != nil {
		t.Fatalf("job = %s %s %s %s fire_key=%v", typ, executor, status, ws, fk)
	}
	if strings.ReplaceAll(payload, " ", "") != `{"workbook_id":"wb-enrich-1","column_id":"src_icp","workspace_id":"ws-alpha"}` {
		t.Fatalf("payload = %s", payload)
	}
}

func TestSourceRunRateLimit(t *testing.T) {
	e := newSrcEnv(t, nil)
	for i := 1; i <= 21; i++ {
		code, resp := e.call("POST", "/api/workbooks/wb-enrich-1/sources/src_icp/run", "alice", "ws-alpha", "")
		want := 200
		if i == 21 {
			want = 429
		}
		if code != want {
			t.Fatalf("call %d = %d %s, want %d", i, code, resp.Body, want)
		}
	}
	if n := e.jobs(t); n != 20 {
		t.Fatalf("jobs = %d, want 20", n)
	}
}

func TestSourceForeignWorkbook404(t *testing.T) {
	e := newSrcEnv(t, nil)
	for _, path := range []string{"/api/workbooks/wb-enrich-1/sources", "/api/workbooks/wb-enrich-1/sources/src_icp/run"} {
		code, resp := e.call("POST", path, "bob", "ws-beta", `{"name":"x"}`)
		if code != 404 || string(resp.Body) != `{"detail":"Workbook not found"}` {
			t.Fatalf("%s = %d %s", path, code, resp.Body)
		}
	}
	if e.jobs(t) != 0 || len(e.columns(t, "wb-enrich-1")) != 2 {
		t.Fatal("a foreign workspace changed state")
	}
}

func TestPreviewNotRegisteredInGo(t *testing.T) {
	e := newSrcEnv(t, nil)
	for _, p := range e.router.Patterns() {
		if strings.Contains(p, "/preview") {
			t.Fatalf("preview must stay on FastAPI, registered: %s", p)
		}
	}
	code, _ := e.call("GET", "/api/workbooks/wb-enrich-1/sources/src_icp/preview", "alice", "ws-alpha", "")
	if code == 200 {
		t.Fatalf("preview answered 200 in Go")
	}
}

// TestAddSourceCanonicalizesNumbers: nested numbers are stored and returned as Python would (1e2 -> 100.0, 1.50 -> 1.5).
func TestAddSourceCanonicalizesNumbers(t *testing.T) {
	e := newSrcEnv(t, nil)
	code, resp := e.call("POST", "/api/workbooks/wb-enrich-big/sources", "alice", "ws-alpha", `{"icp":{"n":1e2,"m":1.50}}`)
	if code != 200 || !strings.Contains(string(resp.Body), `"icp":{"n":100.0,"m":1.5}`) {
		t.Fatalf("response %d %s", code, resp.Body)
	}
	var raw string
	err := tenancy.InWorkspace(context.Background(), e.pool, "ws-alpha", func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT columns_config::text FROM workbooks WHERE id='wb-enrich-big'`).Scan(&raw)
	})
	if err != nil || !strings.Contains(raw, `100.0`) || strings.Contains(raw, `1e2`) || strings.Contains(raw, `1.50`) {
		t.Fatalf("stored %s (%v)", raw, err)
	}
}
