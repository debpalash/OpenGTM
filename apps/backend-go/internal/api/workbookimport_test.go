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
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// importCaseOrder is the order contract_cases/import.json was recorded in: the
// import cases write state that later cases build on.
var importCaseOrder = []string{
	"p_no_token", "p_non_editor", "p_unknown_workbook", "p_foreign_workbook", "p_missing_rows", "p_empty_rows",
	"p_rows_not_dicts", "p_bad_types", "p_file_name_too_long", "p_mapping_not_object", "p_body_not_object",
	"p_no_body", "i_no_token", "i_non_editor", "i_unknown_workbook", "i_foreign_workbook", "i_missing_rows",
	"i_empty_rows", "i_rows_not_dicts", "i_bad_types", "i_file_name_too_long", "i_mapping_not_object",
	"i_body_not_object", "i_no_body", "i_all_empty_rows", "i_501_columns", "i_all_empty_501_columns", "p_clay",
	"p_clay_headers_only", "p_mapping", "p_create_columns_off", "p_collisions", "p_unicode", "p_all_empty",
	"p_as_editor", "p_generic_explicit", "i_first", "i_dedupe_again", "i_dedupe_off", "i_clay_into_alpha2",
	"i_mapping", "i_collisions", "i_create_columns_off", "i_unicode", "i_as_editor",
	"i_second_import_overwrites_last",
}

func newImportEnv(t *testing.T, budgetMB ...int) rowsEnv {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	e := newRowsEnv(t) // owner pool, cleanup, seed helpers, the app pool (one advisory lock per test)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(e.pool, nil))
	RegisterWorkbookImport(r, Deps{Pool: e.pool, Signer: testSigner, Identity: e.st, Config: config.APIConfig{}, Flags: config.Flags{PGLeadStore: true, ImportMemoryBudgetMB: append(budgetMB, 0)[0]}})
	RegisterWorkbookRows(r, Deps{Pool: e.pool, Signer: testSigner, Identity: e.st, Config: config.APIConfig{}, Flags: config.Flags{PGLeadStore: true}})
	e.h = r.Handler()
	return e
}

func importBodies(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/wbcrud/import_bodies.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func importWho(name string) string {
	switch {
	case strings.Contains(name, "no_token"):
		return ""
	case strings.Contains(name, "non_editor"):
		return "bob"
	case strings.HasSuffix(name, "as_editor"):
		return "erin"
	}
	return "alice"
}

var importedAtRe = regexp.MustCompile(`"imported_at":"[^"]*"`)

// importState is the projection legacy/python/scripts/dump_import_state.py dumped.
func (e rowsEnv) importState(t *testing.T, idBase int64) map[string]any {
	t.Helper()
	ctx := context.Background()
	out := map[string]any{}
	rows, err := e.owner.Query(ctx, fmt.Sprintf(`select CASE WHEN id > 25 THEN id - %d + 25 ELSE id END, workbook_id, position, data::text, enrichments::text, lead_id, corroboration_count
		from workbook_rows where workbook_id in ('wb-alpha-1','wb-alpha-2','wb-beta-1') order by id`, idBase))
	if err != nil {
		t.Fatal(err)
	}
	var rs []any
	for rows.Next() {
		var id, pos int64
		var cc *int64
		var wb, data, enr string
		var lead *int64
		if err := rows.Scan(&id, &wb, &pos, &data, &enr, &lead, &cc); err != nil {
			t.Fatal(err)
		}
		var leadV any
		if lead != nil {
			leadV = float64(*lead)
		}
		var ccV any
		if cc != nil {
			ccV = float64(*cc)
		}
		rs = append(rs, map[string]any{"id": float64(id), "workbook_id": wb, "position": float64(pos), "data": data,
			"enrichments": enr, "lead_id": leadV, "corroboration_count": ccV})
	}
	rows.Close()
	out["rows"] = rs

	wrows, err := e.owner.Query(ctx, `select id, source_type, status, total_rows, columns_config::text, coalesce(source_config::text,'{}'),
		updated_at > timestamp '2026-08-29 10:00:00' from workbooks where id in ('wb-alpha-1','wb-alpha-2','wb-beta-1') order by id`)
	if err != nil {
		t.Fatal(err)
	}
	var ws []any
	for wrows.Next() {
		var id, st, status, cols, sc string
		var total int64
		var adv bool
		if err := wrows.Scan(&id, &st, &status, &total, &cols, &sc, &adv); err != nil {
			t.Fatal(err)
		}
		cs, err := wbcrud.ParseColumns([]byte(cols))
		if err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(cs))
		for i, c := range cs {
			b, _ := c.MarshalJSON()
			parts[i] = string(b)
		}
		so, err := wbcrud.ParseObject([]byte(sc))
		if err != nil {
			t.Fatal(err)
		}
		sb, _ := so.MarshalJSON()
		ws = append(ws, map[string]any{"id": id, "source_type": st, "status": status, "total_rows": float64(total),
			"columns_config": "[" + strings.Join(parts, ",") + "]", "source_config": importedAtRe.ReplaceAllString(string(sb), `"imported_at":"<ts>"`),
			"updated_at_advanced": adv})
	}
	wrows.Close()
	out["workbooks"] = ws
	return out
}

func TestImportMatchesFixtures(t *testing.T) {
	e := newImportEnv(t)
	idBase := e.seed(t)
	users := e.users(t)
	bodies := importBodies(t)
	for _, name := range importCaseOrder {
		fx := contract.Load(t, "import/"+name)
		body := string(fx.Request.Body)
		if body == "null" {
			body = ""
		}
		if o, ok := bodies[name]; ok {
			body = o
		}
		_, got := e.do(fx.Request.Method, fx.Request.Path, "application/json", body, users[importWho(name)])
		t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
	}
	golden, err := os.ReadFile("../../testdata/wbcrud/import_final_state.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(golden, &want); err != nil {
		t.Fatal(err)
	}
	got := e.importState(t, idBase)
	for _, k := range []string{"rows", "workbooks"} {
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

func TestImportBodyOverCapIs413AndWritesNothing(t *testing.T) {
	e := newImportEnv(t)
	e.seed(t)
	users := e.users(t)
	big := `{"rows":[{"Company":"` + strings.Repeat("x", maxImportBody+1024) + `"}]}`
	rec, _ := e.do("POST", "/api/workbooks/wb-alpha-1/import", "application/json", big, users["alice"])
	if rec.Code != 413 || !strings.Contains(rec.Body.String(), "Request body too large") {
		t.Fatalf("status %d body %.200s", rec.Code, rec.Body.String())
	}
	var n int
	if err := e.owner.QueryRow(context.Background(), `select count(*) from workbook_rows where workbook_id='wb-alpha-1'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows=%d err=%v", n, err)
	}
}

// A refused import must leave the workbook (and its updated_at, which orders the
// sidebar) exactly as it was.
func TestRefusedImportDoesNotTouchWorkbook(t *testing.T) {
	e := newImportEnv(t)
	e.seed(t)
	users := e.users(t)
	var before string
	q := `select updated_at::text || source_config::text || columns_config::text from workbooks where id='wb-alpha-2'`
	if err := e.owner.QueryRow(context.Background(), q).Scan(&before); err != nil {
		t.Fatal(err)
	}
	rec, _ := e.do("POST", "/api/workbooks/wb-alpha-2/import", "application/json", `{"rows":[{"Company":""}]}`, users["alice"])
	if rec.Code != 400 {
		t.Fatalf("status %d", rec.Code)
	}
	var after string
	if err := e.owner.QueryRow(context.Background(), q).Scan(&after); err != nil || after != before {
		t.Fatalf("workbook changed by a refused import: %q -> %q (%v)", before, after, err)
	}
}

func (e rowsEnv) post(path string, body io.Reader, length int64, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, body)
	req.ContentLength = length
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestImportBudget503ThenRecovers(t *testing.T) {
	old := importBudgetWait
	importBudgetWait = 150 * time.Millisecond
	t.Cleanup(func() { importBudgetWait = old })
	e := newImportEnv(t, 64) // 64 MiB: one max upload fills it
	e.seed(t)
	users := e.users(t)
	alice := users["alice"]
	const path = "/api/workbooks/wb-alpha-1/import"

	// a slow max-weight upload (chunked: unknown length) holds the whole budget
	pr, pw := io.Pipe()
	held := make(chan *httptest.ResponseRecorder, 1)
	go func() { held <- e.post(path, pr, -1, alice) }()
	time.Sleep(100 * time.Millisecond)

	rec := e.post("/api/workbooks/wb-alpha-1/import/preview", strings.NewReader(`{"rows":[{"a":1}]}`), 18, alice)
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "5" || !strings.Contains(rec.Body.String(), "Too many imports in progress") {
		t.Fatalf("status %d retry-after %q body %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	// anonymous traffic is refused by auth before it can queue for budget
	if rec := e.post(path, strings.NewReader(`{}`), 2, nil); rec.Code != 401 {
		t.Errorf("anonymous: %d", rec.Code)
	}
	_, _ = pw.Write([]byte(`{"rows":[{"Company":"Held Co"}]}`))
	_ = pw.Close()
	if r := <-held; r.Code != 200 {
		t.Fatalf("held import: %d %s", r.Code, r.Body.String())
	}

	// every outcome returns its bytes: 422, 400, 404, 413 and success, then a max-weight upload still fits
	deep := strings.Repeat("[", 1<<20)
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{path, `{}`, 422}, {path, deep, 422}, {path, `{"rows":[{"a":""}]}`, 400},
		{"/api/workbooks/nope/import", `{"rows":[{"a":1}]}`, 404}, {path, `{"rows":[{"Company":"After Co"}]}`, 200},
	} {
		if rec := e.post(c.path, strings.NewReader(c.body), int64(len(c.body)), alice); rec.Code != c.want {
			t.Errorf("%s %.30s: %d want %d (%s)", c.path, c.body, rec.Code, c.want, rec.Body.String())
		}
	}
	if rec := e.post(path, strings.NewReader(`{}`), maxImportBody+1, alice); rec.Code != 413 {
		t.Errorf("declared oversize: %d", rec.Code)
	}
	if rec := e.post(path, strings.NewReader(`{"rows":[{"Company":"Last Co"}]}`), -1, alice); rec.Code != 200 {
		t.Fatalf("budget leaked: %d %s", rec.Code, rec.Body.String())
	}
	_ = http.StatusOK
}

// the shared ordered decoder also guards the other body routes (add rows here)
func TestDeepBodyOnAddRowsIs422(t *testing.T) {
	e := newImportEnv(t)
	e.seed(t)
	users := e.users(t)
	deep := `{"rows":[{"a":` + strings.Repeat("[", 1<<20)
	rec, _ := e.do("POST", "/api/workbooks/wb-alpha-1/rows", "application/json", deep, users["alice"])
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "json_invalid") {
		t.Fatalf("%d %.200s", rec.Code, rec.Body.String())
	}
}
