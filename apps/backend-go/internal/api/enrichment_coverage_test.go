package api

import (
	"bufio"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

const retainedEnrichmentFile = "../../testdata/contract/retained_enrichment_routes.txt"

func readRouteList(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out[l] = true
	}
	return out
}

var (
	pyEnrichRoute = regexp.MustCompile(`(?m)^@(\w+)\.(get|post|put|delete|patch|websocket)\(\s*"([^"]*)"`)
	pyParam       = regexp.MustCompile(`\{[^}]*\}`)
)

// normRoute compares routes across the two stacks: "WS" is a GET upgrade, parameter names differ
// ({lead_id} vs {row_id}) and a trailing slash is FastAPI's collection form.
func normRoute(r string) string {
	m, p, _ := strings.Cut(r, " ")
	if m == "WS" {
		m = "GET"
	}
	p = pyParam.ReplaceAllString(p, "{}")
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	return m + " " + p
}

// pythonEnrichmentRouters are the four routers whose routes plan 06 (with plans 04 and 05) splits.
func pythonEnrichmentRoutes(t *testing.T) []string {
	t.Helper()
	var routes []string
	for _, name := range []string{"workbooks.py", "ingest.py", "meta.py", "signals.py"} {
		p := testutil.LegacyPath(t, "apps", "api", "routers", name)
		b, err := os.ReadFile(p)
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
		for _, m := range pyEnrichRoute.FindAllStringSubmatch(src, -1) {
			pre, ok := prefixes[m[1]]
			if !ok {
				t.Fatalf("%s: decorator on router %q without a parsed prefix", name, m[1])
			}
			method := strings.ToUpper(m[2])
			if method == "WEBSOCKET" {
				method = "WS"
			}
			routes = append(routes, method+" "+pre+m[3])
		}
	}
	return routes
}

// enrichmentGoPatterns is the route table with every plan 04, 05 and 06 group registered and every flag on.
func enrichmentGoPatterns(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, p := range workbookCrudRouter(t).Patterns() {
		out[p] = true
	}
	for p := range leadsGoPatterns(t) {
		out[p] = true
	}
	r := httpx.NewRouter()
	registerEnrich(r, enrichDepsWithPool(t))
	for _, p := range r.Patterns() {
		out[p] = true
	}
	return out
}

func TestEveryEnrichmentRouteIsPortedOrRetained(t *testing.T) {
	routes := pythonEnrichmentRoutes(t)
	goPat := enrichmentGoPatterns(t)
	ported := map[string]bool{}
	for p := range goPat {
		ported[normRoute(p)] = true
	}
	retained := map[string]bool{}
	for _, f := range []string{retainedEnrichmentFile, retainedLeadsFile} {
		for r := range readRouteList(t, f) {
			retained[normRoute(r)] = true
		}
	}
	// generate-column and provider-accuracy moved to llmroutes.go (WP-LLMROUTES, behind GO_LLM_ROUTES_ENABLED).
	for p := range llmRoutePatterns(t) {
		ported[normRoute(p)] = true
	}
	// The source preview is registered behind GO_SOURCE_PREVIEW_ENABLED (plan 15 WP-SOURCEWB).
	for p := range sourcePreviewPatterns(t) {
		ported[normRoute(p)] = true
	}
	// Plan 06 scope: 14 routes registered by the enrichment groups; nothing retained any more.
	enrich := readRouteList(t, retainedEnrichmentFile)
	if len(enrich) != 0 {
		t.Errorf("retained_enrichment_routes.txt has %d routes, want 0", len(enrich))
	}
	if len(enrichPatterns) != 14 {
		t.Errorf("enrichPatterns has %d routes, want 14", len(enrichPatterns))
	}
	seen := map[string]bool{}
	for _, r := range routes {
		n := normRoute(r)
		seen[n] = true
		if !ported[n] && !retained[n] {
			t.Errorf("Python route in neither a Go router nor a retained list: %s", r)
		}
	}
	for p := range enrichPatterns {
		if !seen[normRoute(p)] {
			t.Errorf("ported enrichment route has no Python counterpart: %s", p)
		}
	}
	var stale []string
	for r := range enrich {
		if !seen[normRoute(r)] {
			stale = append(stale, r)
		}
	}
	sort.Strings(stale)
	for _, s := range stale {
		t.Errorf("retained route has no Python route: %s", s)
	}
}

func TestRetainedEnrichmentRoutesNotRegisteredInGo(t *testing.T) {
	goPat := enrichmentGoPatterns(t)
	for r := range readRouteList(t, retainedEnrichmentFile) {
		if goPat[r] {
			t.Errorf("retained route registered in Go: %s", r)
		}
	}
	// The scaffold's own retained table and the file must agree.
	file := readRouteList(t, retainedEnrichmentFile)
	for _, r := range retainedEnrichRoutes {
		if !file[r] {
			t.Errorf("%s is retained in enrichscaffold_test.go but missing from retained_enrichment_routes.txt", r)
		}
	}
}

var (
	pyJobKey    = regexp.MustCompile(`(?m)^\s+"([a-z_]+)":\s*handle_\w+,`)
	goHandlers  = regexp.MustCompile(`(?s)handlers := map\[string\]runner\{(.*?)\n\t\}`)
	goHandlerKy = regexp.MustCompile(`(?m)^\t\t([\w.]+):`)
	goConst     = regexp.MustCompile(`(?m)^\s*(?:const\s+)?(\w+)\s*=\s*"([a-z_]+)"`)
)

// goJobTypes resolves the keys of cmd/worker's handler map to job type strings.
func goJobTypes(t *testing.T) map[string]bool {
	t.Helper()
	main, err := os.ReadFile(filepath.Join("..", "..", "cmd", "worker", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	consts := map[string]string{}
	add := func(prefix, file string) {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range goConst.FindAllStringSubmatch(string(b), -1) {
			consts[prefix+m[1]] = m[2]
		}
	}
	add("", filepath.Join("..", "..", "cmd", "worker", "main.go"))
	add("queue.", filepath.Join("..", "queue", "claim.go"))
	files, _ := filepath.Glob(filepath.Join("..", "ambitionimport", "*.go"))
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			add("ambitionimport.", f)
		}
	}
	blk := goHandlers.FindStringSubmatch(string(main))
	if blk == nil {
		t.Fatal("handlers map not found in cmd/worker/main.go")
	}
	out := map[string]bool{}
	for _, m := range goHandlerKy.FindAllStringSubmatch(blk[1], -1) {
		v, ok := consts[m[1]]
		if !ok {
			t.Fatalf("cannot resolve job type constant %s", m[1])
		}
		out[v] = true
	}
	return out
}

// committedWorkerJobTypes are the 17 job types the Go worker registers since the plan 15 cutover
// (docs/go-port/runbooks/15-cutover.md). Each area's own coverage test checks its file's handler map; the enrichment
// ones are checked against cmd/worker/main.go here.
var committedWorkerJobTypes = []string{
	"run_workbook", "ambitionbox_import", "refresh_workbook", "signal_scan", "source_workbook", "trigger_eval", "watch_poll",
	"send", "outreach_inbound_poll", "retention_enforce", "research_playbook_run", "research_playbook_schedule",
	"audience_refresh", "audience_destination_sync", "source_health_check", "collect", "bulk_enrich",
}

func TestEnrichmentJobTypesHaveAGoHandler(t *testing.T) {
	known := map[string]bool{}
	for _, ty := range committedWorkerJobTypes {
		known[ty] = true
	}
	handled := goJobTypes(t)
	for _, ty := range []string{"run_workbook", "refresh_workbook", "signal_scan"} {
		if !handled[ty] {
			t.Errorf("plan 06 job type %s is not dispatched by the Go worker", ty)
		}
	}
	for ty := range handled {
		if !known[ty] {
			t.Errorf("Go worker handles %q, which is not one of the 17 committed job types", ty)
		}
	}
	// The recorded Python registry names the same 17 (skipped without the legacy tree).
	t.Run("python registry", func(t *testing.T) {
		b, err := os.ReadFile(testutil.LegacyPath(t, "apps", "api", "services", "job_registry.py"))
		if err != nil {
			t.Skip(err)
		}
		py := map[string]bool{}
		for _, m := range pyJobKey.FindAllStringSubmatch(string(b), -1) {
			py[m[1]] = true
		}
		if len(py) != len(known) {
			t.Fatalf("Python registered %d job types, want %d", len(py), len(known))
		}
		for ty := range py {
			if !known[ty] {
				t.Errorf("Python registers %q, missing from committedWorkerJobTypes", ty)
			}
		}
	})
}

// sourcePreviewPatterns are the routes registerWorkbookSources adds with GO_SOURCE_PREVIEW_ENABLED on
// (beyond the always-on add and run routes).
func sourcePreviewPatterns(t *testing.T) map[string]bool {
	t.Helper()
	r := httpx.NewRouter()
	registerWorkbookSources(r, enrichDepsWithPool(t))
	out := map[string]bool{}
	for _, p := range r.Patterns() {
		if strings.HasSuffix(p, "/preview") {
			out[p] = true
		}
	}
	if len(out) != 1 || !out["GET /api/workbooks/{workbook_id}/sources/{col_id}/preview"] {
		t.Errorf("preview flag adds %v, want only the source preview", out)
	}
	return out
}
