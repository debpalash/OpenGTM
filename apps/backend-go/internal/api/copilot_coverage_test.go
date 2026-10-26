package api

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/copilot"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
)

// Plan 12 coverage (Task 21): the 7 copilotkit + 5 person intel FastAPI routes are all ported (none retained),
// each has a recorded fixture, and the retained list is explicit.

const retainedCopilotFile = "../../testdata/contract/retained_copilot_routes.txt"

// wsRouteTests names the tests that cover a WebSocket route: the recorder (contract_record_ws.py) only
// drives workbook sockets and person enrichment needs live network scraping, so there is no recorded
// fixture; the route is pinned by these integration tests instead (checked to exist by name).
var wsRouteTests = map[string][]string{
	"GET /ws/person/enrich": {"TestPersonWSRejectsBadTokenOrNonAdmin4403", "TestPersonWSHappyPathStepsMatchFixture",
		"TestPersonWSDisconnectCancelsPipeline", "TestPersonWSDeadline"},
}

var pyWebsocketRoute = regexp.MustCompile(`(?m)^@(\w+)\.websocket\(\s*"([^"]*)"`)

func copilotGoPatterns(t *testing.T) map[string]bool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://coverage@127.0.0.1:1/coverage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	d := Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Pool: pool, Identity: identity.NewStore(pool),
		PyWorker: pyworker.New("http://127.0.0.1:1", "tok", nil),
		Flags:    config.Flags{PGLeadStore: true, Copilot: config.CopilotSettings{GoCopilotEnabled: true}}}
	out := map[string]bool{}
	for _, register := range []func(*httpx.Router, Deps){RegisterCopilot, RegisterPersonIntel} {
		r := httpx.NewRouter()
		register(r, d)
		for _, p := range r.Patterns() {
			out[p] = true
		}
	}
	return out
}

func readRetainedCopilot(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(retainedCopilotFile)
	if err != nil {
		t.Fatalf("retained routes list missing: %v", err)
	}
	out := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out[l] = true
		}
	}
	return out
}

// pythonCopilotRoutes parses routers/copilotkit.py (prefix /api/copilotkit) and routers/person_intel.py.
func pythonCopilotRoutes(t *testing.T) (copilotKit, personIntel []string) {
	t.Helper()
	parse := func(name string) []string {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "legacy", "python", "apps", "api", "routers", name))
		if err != nil {
			t.Skipf("legacy Python tree not present (%v)", err)
		}
		src := string(b)
		prefixes := map[string]string{}
		for _, m := range pyRouterDecl.FindAllStringSubmatch(src, -1) {
			prefixes[m[1]] = ""
			if pm := pyPrefix.FindStringSubmatch(m[2]); pm != nil {
				prefixes[m[1]] = pm[1]
			}
		}
		var routes []string
		for _, m := range pyRoute.FindAllStringSubmatch(src, -1) {
			pre, ok := prefixes[m[1]]
			if !ok {
				t.Fatalf("%s: decorator on router %q without a router declaration", name, m[1])
			}
			routes = append(routes, strings.ToUpper(m[2])+" "+pre+m[3])
		}
		for _, m := range pyWebsocketRoute.FindAllStringSubmatch(src, -1) {
			routes = append(routes, "GET "+prefixes[m[1]]+m[2])
		}
		sort.Strings(routes)
		return routes
	}
	return parse("copilotkit.py"), parse("person_intel.py")
}

func TestEveryCopilotAndPersonRouteIsPorted(t *testing.T) {
	ck, pi := pythonCopilotRoutes(t)
	if len(ck) != 7 || len(pi) != 5 {
		t.Fatalf("want 7 copilotkit + 5 person_intel Python routes, parsed %d + %d: %v %v", len(ck), len(pi), ck, pi)
	}
	ported := copilotGoPatterns(t)
	retained := readRetainedCopilot(t)
	if len(ported) != 12 || len(retained) != 0 {
		t.Errorf("want 12 ported + 0 retained, got %d + %d", len(ported), len(retained))
	}
	seen := map[string]bool{}
	for _, r := range append(append([]string{}, ck...), pi...) {
		seen[r] = true
		if !ported[r] && !retained[r] {
			t.Errorf("Python route in neither the Go router nor the retained list: %s", r)
		}
		if ported[r] && retained[r] {
			t.Errorf("route both ported and retained: %s", r)
		}
	}
	for r := range ported {
		if !seen[r] {
			t.Errorf("Go route with no Python counterpart: %s", r)
		}
	}
	for r := range retained {
		if !seen[r] {
			t.Errorf("stale retained entry: %s", r)
		}
	}
}

func TestEveryCopilotAndPersonRouteHasFixtureOrIsRetained(t *testing.T) {
	ported := copilotGoPatterns(t)
	retained := readRetainedCopilot(t)
	reqs := fixtureRequests(t, "copilot", "person")
	rest := map[string]bool{}
	for p := range ported {
		if retained[p] {
			continue
		}
		if tests, ws := wsRouteTests[p]; ws {
			src, err := os.ReadFile("personintelws_integration_test.go")
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range tests {
				if !strings.Contains(string(src), "func "+name+"(") {
					t.Errorf("%s: covering test %s does not exist", p, name)
				}
			}
			continue
		}
		rest[p] = true
	}
	assertFixturesCover(t, rest, reqs)
}

func TestEveryCopilotCatalogToolHasExecutor(t *testing.T) {
	native := map[string]bool{}
	for _, n := range copilot.NativeToolNames() {
		native[n] = true
	}
	for _, tl := range copilot.Catalog() {
		switch tl.Executor {
		case "go":
			if !native[tl.Name] {
				t.Errorf("%s is executor=go without a Go implementation", tl.Name)
			}
		case "python":
		default:
			t.Errorf("%s has executor %q", tl.Name, tl.Executor)
		}
	}
}

// The deviations the runbook and README promise operators must be written down.
func TestCopilotDeviationsDocumented(t *testing.T) {
	b, err := os.ReadFile("../../testdata/contract/README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Plan 12 (copilot", "COPILOT_GO_ENABLED", "approved_tool_calls", "person_intel", "114"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("README missing plan 12 deviation text %q", want)
		}
	}
	if _, err := os.Stat("../../../../docs/go-port/runbooks/copilot-cutover.md"); err != nil {
		t.Errorf("runbook missing: %v", err)
	}
}
