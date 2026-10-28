package api

import (
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testutil"
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
	root := testutil.LegacyPath(t, "apps", "api", "routers")
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
	registerLeadgen(r, leadgenDeps(true, true))
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
	// POST /api/lead/{lead_id}/enrich moved to llmroutes.go (WP-LLMROUTES, behind GO_LLM_ROUTES_ENABLED).
	for p := range llmRoutePatterns(t) {
		if strings.HasSuffix(p, "/enrich") {
			ported[normRoute(p)] = true
		}
	}
	// GET /api/v2/search/unified and POST /api/v2/scraper/scrape moved to scrape.go (WP-ROUTES-B, GO_SCRAPE_ROUTES_ENABLED).
	for p := range scrapeRoutePatterns(t) {
		if strings.Contains(p, "/api/v2/search/unified") || strings.Contains(p, "/api/v2/scraper/scrape") {
			ported[normRoute(p)] = true
		}
	}
	if len(ported) != 22 || len(retainedList) != 0 {
		t.Errorf("want 22 ported + 0 retained leadgen routes, got %d + %d", len(ported), len(retainedList))
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

// Plan 15 cutover: every leadgen job type runs in the Go worker. source_health_check's executor is go whatever the
// (retired) flags say, and its worker handler no longer hands anything back to Python.
func TestLeadgenJobTypesAlwaysRouteToGo(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "cmd", "worker", "handlers_leadgen.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "handBack") {
		t.Error("handlers_leadgen.go still hands source_health_check back to Python")
	}
	for _, e := range []config.ExecutorSettings{{}, {Enabled: true}, {SourceHealth: true}, {Enabled: true, SourceHealth: true}} {
		if got := e.SourceHealthExecutor(); got != "go" {
			t.Errorf("SourceHealthExecutor(%+v) = %s, want go", e, got)
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
		"../domainintel", "../leadjobs", "../../cmd/worker", "../../cmd/api", ".",
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
