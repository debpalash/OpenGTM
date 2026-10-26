package api

import (
	"bufio"
	"context"
	"encoding/json"
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
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

const (
	retainedConnectorFile = "../../testdata/contract/retained_connector_routes.txt"
	contractREADME        = "../../testdata/contract/README.md"
)

var connectorFixtureDirs = []string{"connectors", "hubspot", "datacrm", "ambitionbox"}

func readRetainedConnectors(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open(retainedConnectorFile)
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

func connectorGoPatterns(t *testing.T) map[string]bool {
	t.Helper()
	// pgxpool.New is lazy: no connection is made, the pool only has to be non-nil.
	pool, err := pgxpool.New(context.Background(), "postgres://coverage@127.0.0.1:1/coverage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	env, err := secrets.NewEnvelope(secrets.Config{MasterKey: "coverage-test-key", AppEnv: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Pool: pool, Envelope: env,
		Flags: config.Flags{PGLeadStore: true, GoConnectorRoutesEnabled: true}}
	r := httpx.NewRouter()
	RegisterConnectors(r, d)
	RegisterHubspot(r, d)
	RegisterDataCRM(r, d)
	RegisterAmbitionBox(r, d)
	out := map[string]bool{}
	for _, p := range r.Patterns() {
		out[p] = true
	}
	return out
}

var pyConnectorRouterPrefix = regexp.MustCompile(`(?s)(\w+)\s*=\s*APIRouter\(.{0,300}?prefix\s*=\s*"([^"]*)"`)

// pythonConnectorRoutes parses the route decorators of the four in-scope routers
// (connectors.py, hubspot.py, ambitionbox.py, crm.py = /api/data).
func pythonConnectorRoutes(t *testing.T) []string {
	t.Helper()
	var routes []string
	for _, name := range []string{"connectors.py", "hubspot.py", "ambitionbox.py", "crm.py"} {
		p := filepath.Join("..", "..", "..", "..", "legacy", "python", "apps", "api", "routers", name)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Skipf("legacy Python tree not present (%v)", err)
		}
		src := string(b)
		prefixes := map[string]string{}
		for _, m := range pyConnectorRouterPrefix.FindAllStringSubmatch(src, -1) {
			prefixes[m[1]] = m[2]
		}
		for _, m := range pyRoute.FindAllStringSubmatch(src, -1) {
			pre, ok := prefixes[m[1]]
			if !ok {
				t.Fatalf("%s: decorator on router %q without a parsed prefix", name, m[1])
			}
			routes = append(routes, strings.ToUpper(m[2])+" "+pre+m[3])
		}
	}
	return routes
}

func TestEveryPythonConnectorRouteIsPortedOrRetained(t *testing.T) {
	routes := pythonConnectorRoutes(t)
	ported, retained := connectorGoPatterns(t), readRetainedConnectors(t)
	if len(ported) != 19 || len(retained) != 0 {
		t.Errorf("want 19 ported + 0 retained routes, got %d + %d", len(ported), len(retained))
	}
	if len(routes) != 19 {
		t.Errorf("want 19 Python route decorators, parsed %d", len(routes))
	}
	seen := map[string]bool{}
	for _, r := range routes {
		seen[r] = true
		if !ported[r] && !retained[r] {
			t.Errorf("route in neither Go router nor retained list: %s", r)
		}
		if ported[r] && retained[r] {
			t.Errorf("route both ported and retained: %s", r)
		}
	}
	var stale []string
	for r := range ported {
		if !seen[r] {
			stale = append(stale, "ported "+r)
		}
	}
	for r := range retained {
		if !seen[r] {
			stale = append(stale, "retained "+r)
		}
	}
	sort.Strings(stale)
	for _, s := range stale {
		t.Errorf("no such Python route: %s", s)
	}
}

func TestRetainedConnectorRoutesNotRegisteredInGo(t *testing.T) {
	ported := connectorGoPatterns(t)
	for r := range readRetainedConnectors(t) {
		if ported[r] {
			t.Errorf("retained route registered in Go: %s", r)
		}
	}
}

// TestEveryConnectorRouteHasFixture: every registered pattern has at least one recorded
// FastAPI fixture (method + path matching the pattern) in the four plan 07 fixture dirs.
func TestEveryConnectorRouteHasFixture(t *testing.T) {
	type fx struct {
		Request struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"request"`
	}
	var reqs []string
	for _, dir := range connectorFixtureDirs {
		files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "contract", dir, "*.json"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no fixtures in %s (%v)", dir, err)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var x fx
			if err := json.Unmarshal(b, &x); err != nil || x.Request.Method == "" {
				continue // not a request/response fixture (meta file)
			}
			path := x.Request.Path
			if i := strings.IndexByte(path, '?'); i >= 0 {
				path = path[:i]
			}
			reqs = append(reqs, strings.ToUpper(x.Request.Method)+" "+path)
		}
	}
	for p := range connectorGoPatterns(t) {
		re := regexp.MustCompile("^" + regexp.MustCompile(`\\\{[^}]+\\\}`).ReplaceAllString(regexp.QuoteMeta(p), `[^/]+`) + "$")
		found := false
		for _, q := range reqs {
			if re.MatchString(q) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no recorded fixture for %s", p)
		}
	}
}

// TestDeviationsDocumented: the plan 07 open decisions D3-D5 and the carried documentation
// items are present in testdata/contract/README.md.
func TestDeviationsDocumented(t *testing.T) {
	b, err := os.ReadFile(contractREADME)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	i := strings.Index(doc, "## Plan 07 coverage and rollout (Task 19)")
	if i < 0 {
		t.Fatal("README lacks the plan 07 Task 19 section")
	}
	sec := doc[i:]
	for _, want := range []string{"D3", "D4", "D5", "Plan 07 fixture recording procedure", "naive", "retained_connector_routes.txt"} {
		if !strings.Contains(sec, want) {
			t.Errorf("plan 07 README section does not mention %q", want)
		}
	}
	for _, want := range []string{"## Certification (plan 07 Task 4): deviations", "## Email data routes (plan 07 Task 17): deviations"} {
		if !strings.Contains(doc, want) {
			t.Errorf("README lacks %q", want)
		}
	}
}
