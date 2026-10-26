package api

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

const retainedLeadgenFile = "../../testdata/contract/retained_leadgen_routes.txt"

// leadgenUtilitySuffixes are the /api/leads/* utilities plan 14 owns; the rest of /api/leads/* is plan 05.
var leadgenUtilityPaths = map[string]bool{
	"/api/leads/verify-email": true, "/api/leads/score": true,
	"/api/leads/tech-stack": true, "/api/leads/domain-intel": true,
}

// pythonLeadgenRoutes parses the plan 14 slice of routers/leads.py (jobs_router, the four /leads/*
// utilities, the retained enrich and search_router routes) and the /sources* routes of settings.py.
func pythonLeadgenRoutes(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..", "..", "..", "legacy", "python", "apps", "api", "routers")
	var routes []string
	for _, name := range []string{"leads.py", "settings.py"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Skipf("legacy Python tree not present (%v)", err)
		}
		src := string(b)
		prefixes := map[string]string{}
		for _, m := range pyRouterDecl.FindAllStringSubmatch(src, -1) {
			pre := ""
			if pm := pyPrefix.FindStringSubmatch(m[2]); pm != nil {
				pre = pm[1]
			}
			prefixes[m[1]] = pre
		}
		for _, m := range pyRoute.FindAllStringSubmatch(src, -1) {
			pre, ok := prefixes[m[1]]
			if !ok {
				t.Fatalf("%s: decorator on unknown router %q", name, m[1])
			}
			full := pre + m[3]
			method := strings.ToUpper(m[2])
			in := false
			switch {
			case name == "settings.py":
				in = full == "/api/settings/sources" || strings.HasPrefix(full, "/api/settings/sources/")
			case m[1] == "jobs_router" || m[1] == "search_router":
				in = true
			case m[1] == "router":
				in = leadgenUtilityPaths[full] || (method == "POST" && full == "/api/lead/{lead_id}/enrich")
			}
			if in {
				routes = append(routes, method+" "+full)
			}
		}
	}
	return routes
}

// leadgenGoPatterns is every plan 14 route registered with all flags on.
func leadgenGoPatterns(t *testing.T) map[string]bool {
	t.Helper()
	r := httpx.NewRouter()
	registerLeadgen(r, leadgenDeps(true, true, "postgres"))
	out := map[string]bool{}
	for _, p := range r.Patterns() {
		out[p] = true
	}
	return out
}

func TestEveryLeadgenRouteIsPortedOrRetained(t *testing.T) {
	routes := pythonLeadgenRoutes(t)
	ported := map[string]bool{}
	for p := range leadgenGoPatterns(t) {
		ported[normRoute(p)] = true
	}
	retainedList := readRouteList(t, retainedLeadgenFile)
	retained := map[string]bool{}
	for r := range retainedList {
		retained[normRoute(r)] = true
	}
	if len(ported) != 19 || len(retainedList) != 3 {
		t.Errorf("want 19 ported + 3 retained leadgen routes, got %d + %d", len(ported), len(retainedList))
	}
	if len(routes) != 22 {
		t.Errorf("want 22 Python route decorators in scope, parsed %d: %v", len(routes), routes)
	}
	seen := map[string]bool{}
	for _, r := range routes {
		n := normRoute(r)
		seen[n] = true
		if !ported[n] && !retained[n] {
			t.Errorf("Python route in neither a Go router nor retained_leadgen_routes.txt: %s", r)
		}
		if ported[n] && retained[n] {
			t.Errorf("route both ported and retained: %s", r)
		}
	}
	var stale []string
	for p := range ported {
		if !seen[p] {
			stale = append(stale, "ported "+p)
		}
	}
	for p := range retained {
		if !seen[p] {
			stale = append(stale, "retained "+p)
		}
	}
	sort.Strings(stale)
	for _, s := range stale {
		t.Errorf("no such Python route: %s", s)
	}
}

func TestRetainedLeadgenRoutesNotRegisteredInGo(t *testing.T) {
	goPat := leadgenGoPatterns(t)
	norm := map[string]bool{}
	for p := range goPat {
		norm[normRoute(p)] = true
	}
	// Every other Go group too: a retained path must not be answered by any Go router.
	for p := range enrichmentGoPatterns(t) {
		norm[normRoute(p)] = true
	}
	for r := range readRouteList(t, retainedLeadgenFile) {
		if norm[normRoute(r)] {
			t.Errorf("retained route registered in Go: %s", r)
		}
	}
}

// Every Go-registered leadgen route has a recorded FastAPI fixture (collect, jobs, leadutils and
// the srcsettings_* files of sources); the retained routes are exempt. tech-stack and verify-email
// success paths are replayed from provider transcripts (README), the routes themselves still need
// a recorded request.
func TestEveryLeadgenRouteHasFixtureOrIsRetained(t *testing.T) {
	retained := map[string]bool{}
	for r := range readRouteList(t, retainedLeadgenFile) {
		retained[r] = true
	}
	need := map[string]bool{}
	for p := range leadgenGoPatterns(t) {
		if !retained[p] {
			need[p] = true
		}
	}
	assertFixturesCover(t, need, fixtureRequests(t, "collect", "jobs", "leadutils", "sources"))
}

// The collection execution stays in the Python worker: collect, bulk_enrich and source_workbook are
// never dispatched by the Go worker; source_health_check is, but only behind its flag (handBackInert
// otherwise, covered by cmd/worker TestLeadgenHandlers*).
func TestLeadgenJobTypesHandledOrPython(t *testing.T) {
	handled := goJobTypes(t)
	for _, ty := range []string{"collect", "bulk_enrich", "source_workbook"} {
		if handled[ty] {
			t.Errorf("job type %s must dispatch to the Python worker, not the Go worker", ty)
		}
	}
	// source_health_check is registered by cmd/worker/handlers_leadgen.go (not in the main map);
	// its gate must be the master executor flag AND GO_EXECUTOR_SOURCE_HEALTH.
	b, err := os.ReadFile(filepath.Join("..", "..", "cmd", "worker", "handlers_leadgen.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{"SourceHealthExecutor() != \"go\"", "handBackInert(pool, \"GO_EXECUTOR_SOURCE_HEALTH is off\")"} {
		if !strings.Contains(src, want) {
			t.Errorf("handlers_leadgen.go no longer gates source_health_check: missing %q", want)
		}
	}
	for _, c := range []struct {
		e    config.ExecutorSettings
		want string
	}{
		{config.ExecutorSettings{}, "python"},
		{config.ExecutorSettings{Enabled: true}, "python"},
		{config.ExecutorSettings{SourceHealth: true}, "python"},
		{config.ExecutorSettings{Enabled: true, SourceHealth: true}, "go"},
	} {
		if got := c.e.SourceHealthExecutor(); got != c.want {
			t.Errorf("SourceHealthExecutor(%+v) = %s, want %s", c.e, got, c.want)
		}
	}
}

// scraperImports are the scraper-class dependencies that must never appear in a Go handler: HTML
// scraping, browser automation and crawl frameworks stay in the Python worker (constraints.md).
var scraperImports = []string{
	"github.com/gocolly/", "github.com/chromedp/", "github.com/go-rod/", "github.com/PuerkitoBio/goquery",
	"github.com/playwright-community/", "github.com/anaskhan96/soup", "golang.org/x/net/html",
	"github.com/geziyor/", "github.com/tebeka/selenium",
}

func TestNoGoHandlerImportsScraperDeps(t *testing.T) {
	dirs := []string{
		"../collectintent", "../collectledger", "../leadscore", "../leadprogress", "../sourcehealth", "../sourcecatalog",
		"../domainintel", "../leadjobs", "../pyworker", "../../cmd/worker", "../../cmd/api", ".",
	}
	var files []string
	for _, d := range dirs {
		fs, err := filepath.Glob(filepath.Join(d, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, fs...)
	}
	if len(files) < 40 {
		t.Fatalf("only %d Go files scanned, the package list is stale", len(files))
	}
	fset := token.NewFileSet()
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		n++
		for _, imp := range af.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range scraperImports {
				if strings.HasPrefix(path, bad) {
					t.Errorf("%s imports scraper-class dependency %s (scrapers stay Python)", f, path)
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("no non-test files scanned")
	}
}
