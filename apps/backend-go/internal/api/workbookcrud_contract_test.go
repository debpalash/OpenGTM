package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

// Plan 04 route coverage. The per-area replay tests (workbookroutes_test.go and
// friends) compare Go responses with the recorded FastAPI fixtures; the tests
// here pin that no migrated route lacks a fixture, that the retained routes are
// not registered, and that every client-visible guard string is recorded.

// workbookCrudAreas are the fixture directories owned by plan 04.
var workbookCrudAreas = []string{
	"workbooks", "columns", "rows", "rows_recompute", "detail", "export", "import", "views",
	"telemetry", "meta", "templates", "functions", "entities", "entities_write", "analytics", "system",
}

// enrichmentContractAreas are the fixture directories owned by plan 06 (enrichment_coverage_test.go
// checks that each of its Go routes has one).
var enrichmentContractAreas = []string{"estimate", "run", "cellrun", "sources", "refresh", "migrate", "meta", "ingest", "ws"}

// workbookCrudPrefixes selects the plan 04 routes out of the whole route table.
var workbookCrudPrefixes = []string{
	"/api/workbooks", "/api/v2/workbooks", "/api/templates", "/api/functions", "/api/entities",
	"/api/analytics", "/api/me/context", "/api/flags", "/api/system/stats", "/api/files/",
}

func isWorkbookCrudRoute(p string) bool {
	_, path, _ := strings.Cut(p, " ")
	for _, pre := range workbookCrudPrefixes {
		if strings.HasPrefix(path, pre) {
			return true
		}
	}
	return false
}

// workbookCrudRouter registers every plan 04 group against a lazy pool: handlers
// are never invoked, only the route table is inspected.
func workbookCrudRouter(t *testing.T) *httpx.Router {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	d := Deps{Pool: pool, Identity: identity.NewStore(pool), Flags: config.Flags{PGLeadStore: true}, DataDir: t.TempDir()}
	r := httpx.NewRouter()
	RegisterWorkbooks(r, d)
	RegisterWorkbookDetail(r, d)
	RegisterWorkbookColumns(r, d)
	RegisterWorkbookRows(r, d)
	RegisterWorkbookImport(r, d)
	RegisterWorkbookViews(r, d)
	RegisterWorkbookTelemetry(r, d)
	RegisterWorkbookMeta(r, d)
	RegisterTemplates(r, d)
	RegisterFunctions(r, d)
	RegisterEntities(r, d)
	RegisterAnalytics(r, d)
	RegisterSystem(r, d)
	return r
}

func retainedWorkbookRoutes(t *testing.T) []string {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", "contract", "retained_workbook_routes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

func TestRetainedWorkbookRoutesList(t *testing.T) {
	got := retainedWorkbookRoutes(t)
	if len(got) != 16 { // 14 workbook routes + 2 ingest routes
		t.Fatalf("retained list has %d routes, want 16 (14 workbook + 2 ingest)", len(got))
	}
}

func TestEveryMigratedWorkbookRouteHasAFixture(t *testing.T) {
	var want []string
	for _, p := range workbookCrudRouter(t).Patterns() {
		if isWorkbookCrudRoute(p) {
			want = append(want, p)
		}
	}
	if len(want) < 40 {
		t.Fatalf("only %d plan 04 routes registered; registration is incomplete", len(want))
	}
	covered := map[string]bool{}
	for _, area := range workbookCrudAreas {
		for _, name := range contractFixtureNames(t, area) {
			fx := contract.Load(t, name)
			path, _, _ := strings.Cut(fx.Request.Path, "?")
			for _, p := range want {
				if patternRegexp(p).MatchString(fx.Request.Method + " " + path) {
					covered[p] = true
				}
			}
		}
	}
	var missing []string
	for _, p := range want {
		if !covered[p] {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	for _, p := range missing {
		t.Errorf("route %q has no recorded fixture", p)
	}
}

func TestRetainedWorkbookRoutesNotRegistered(t *testing.T) {
	r := workbookCrudRouter(t)
	registered := map[string]bool{}
	for _, p := range r.Patterns() {
		registered[p] = true
	}
	h := r.Handler()
	for _, p := range retainedWorkbookRoutes(t) {
		if registered[p] {
			t.Errorf("%q must stay on FastAPI but Go registers it", p)
		}
		method, path, _ := strings.Cut(p, " ")
		if method == "WS" {
			method = http.MethodGet
		}
		path = paramSeg.ReplaceAllString(path, "abc")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: %d, want 404/405", method, path, rec.Code)
		}
	}
}

var guardLiteral = regexp.MustCompile(`(?:guard\(\d+,|bad400\(|Detail:|[dD]etail\(w,\s*(?:http\.\w+|\d+),)\s*"((?:[^"\\]|\\.)+)"\s*[,)}]`)

// uncoveredGuardAllowlist lists server-side strings that are not client
// contract (hardened deviations or 500 paths) and so have no FastAPI fixture.
var uncoveredGuardAllowlist = map[string]string{
	"Internal Server Error":        "500 path, never recorded",
	"Internal error":               "hardened files/system 500 body (README deviations)",
	"Invalid request body":         "shared decode fallback",
	"Request body too large":       "Go-only hardening (README deviations)",
	"Too many imports in progress": "Go-only import memory budget (README deviations)",
	"Too many exports in progress": "Go-only export concurrency cap (README deviations)",
	"Too many uploads in progress": "Go-only shared upload budget for add rows and workbook create/update (README deviations)",

	"Cannot delete the data directory": "hardened files delete (README deviations)",
	"Directory is not empty":           "hardened files delete (README deviations)",

	// Real FastAPI guards that only fire on corrupted stored state or a race, so the recorder
	// cannot reach them through the API; each has a Go unit test asserting the exact string.
	"Column identity is ambiguous": "duplicate stored column ids; workbookcolumns_test.go",
	"Saved view references a missing filter column. Repair the view before continuing.": "stale saved view; wbcrud/rowquery_unit_test.go",
	"Saved view references a missing sort column. Repair the view before continuing.":   "stale saved view; wbcrud/rowquery_unit_test.go",
	"Selected workbook rows changed. Refresh before running.":                           "concurrent row delete race inside recompute",
	// Cell run route (plan 06 Task 25). "Enrichment column not found" is recorded in the cellrun area
	// (workbookcell_integration_test.go replays it); the others are Go-only answers or need state the
	// recorder cannot seed (a v1 leads-filter lead row, a Python API outage, a client disconnect).
	"Enrichment column not found":  "recorded in the cellrun fixtures (workbookcell_integration_test.go)",
	"Row not found":                "v1 leads-filter row; TestCellRunLegacyLeadRowResolution",
	"Python API is not configured": "Go-only proxy answer (README deviations); TestCellRunProxiesOutputColumnsToPython",
	"Python API unavailable":       "Go-only proxy answer (README deviations); TestCellRunProxiesOutputColumnsToPython",
	"Client Closed Request":        "client disconnect (499); TestCellRunCancelOnClientDisconnect",
	// Plan 06 Go-only hardening (README: Workbook WebSocket, Refresh routes): FastAPI has no connection
	// cap and reads the body through the framework, so neither string is reachable on the reference.
	"Too many websocket connections":     "Go-only WS concurrency cap (503 before upgrade; README deviations), no reference equivalent",
	"Request body could not be read":     "Go-only body-read failure on refresh-policy (README deviations)",
	"Template column IDs must be unique": "embedded template registry is valid; templatesroutes_test.go",
	"Template enrichment column IDs must be explicit and unique. Repair the template before creating a workbook.": "templatesroutes_test.go",
	"Template has circular column dependencies. Repair the template before creating a workbook.":                  "templatesroutes_test.go",
}

func TestFixturesCoverEveryGuardString(t *testing.T) {
	var corpus strings.Builder
	// plan 06 areas count too: the enrichment route files (workbook*.go) are scanned below.
	for _, area := range append(append([]string{}, workbookCrudAreas...), enrichmentContractAreas...) {
		for _, name := range contractFixtureNames(t, area) {
			fx := contract.Load(t, name)
			corpus.Write(fx.Response.Body)
			corpus.WriteByte('\n')
		}
	}
	hay := corpus.String()
	var files []string
	for _, g := range []string{"workbook*.go", "templates*.go", "functions*.go", "entities*.go", "analytics*.go", "system*.go"} {
		m, _ := filepath.Glob(g)
		files = append(files, m...)
	}
	m, _ := filepath.Glob(filepath.Join("..", "wbcrud", "*.go"))
	files = append(files, m...)
	seen := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, mm := range guardLiteral.FindAllStringSubmatch(string(b), -1) {
			if strings.Contains(mm[1], "%") {
				continue
			}
			seen[mm[1]] = filepath.Base(f)
		}
	}
	if len(seen) < 30 {
		t.Fatalf("extracted only %d guard strings; extraction regexp is stale", len(seen))
	}
	var lits []string
	for s := range seen {
		lits = append(lits, s)
	}
	sort.Strings(lits)
	for _, s := range lits {
		if _, ok := uncoveredGuardAllowlist[s]; ok {
			continue
		}
		// fixtures hold JSON-escaped strings
		if !strings.Contains(hay, s) && !strings.Contains(hay, strings.ReplaceAll(s, `"`, `\"`)) {
			t.Errorf("guard string %q (%s) appears in no recorded fixture", s, seen[s])
		}
	}
}
