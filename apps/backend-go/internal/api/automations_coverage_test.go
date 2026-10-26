package api

import (
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
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

const retainedAutomationFile = "../../testdata/contract/retained_automation_routes.txt"

// automationGoPatterns registers the plan 08 routers with every flag on and returns the patterns
// split by router (11 automations, 7 watches).
func automationGoPatterns(t *testing.T) (autos, watches map[string]bool) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://coverage@127.0.0.1:1/coverage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	d := Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Pool: pool, Identity: identity.NewStore(pool),
		Flags: config.Flags{PGLeadStore: true, Automations: config.AutomationsSettings{GoRoutesEnabled: true}}}
	pat := func(register func(*httpx.Router, Deps)) map[string]bool {
		r := httpx.NewRouter()
		register(r, d)
		out := map[string]bool{}
		for _, p := range r.Patterns() {
			out[p] = true
		}
		return out
	}
	return pat(RegisterAutomations), pat(RegisterWatches)
}

func readRetainedAutomations(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(retainedAutomationFile)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out[l] = true
	}
	return out
}

func pythonAutomationRoutes(t *testing.T) []string {
	t.Helper()
	var routes []string
	for _, name := range []string{"automations.py", "watches.py"} {
		p := filepath.Join("..", "..", "..", "..", "legacy", "python", "apps", "api", "routers", name)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Skipf("legacy Python tree not present (%v)", err)
		}
		src := string(b)
		prefixes := map[string]string{}
		for _, m := range pyRouterDecl.FindAllStringSubmatch(src, -1) {
			if pm := pyPrefix.FindStringSubmatch(m[2]); pm != nil {
				prefixes[m[1]] = pm[1]
			}
		}
		for _, m := range pyRoute.FindAllStringSubmatch(src, -1) {
			pre, ok := prefixes[m[1]]
			if !ok {
				t.Fatalf("%s: decorator on router %q without a parsed prefix", name, m[1])
			}
			p := m[3]
			// the Go router spells path params as {name}; Python does too.
			routes = append(routes, strings.ToUpper(m[2])+" "+pre+p)
		}
	}
	return routes
}

// fixtureRequests lists "METHOD path" of every recorded request in the given fixture dirs.
func fixtureRequests(t *testing.T, dirs ...string) []string {
	t.Helper()
	type fx struct {
		Request struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"request"`
	}
	var reqs []string
	for _, dir := range dirs {
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
				continue
			}
			path := x.Request.Path
			if i := strings.IndexByte(path, '?'); i >= 0 {
				path = path[:i]
			}
			reqs = append(reqs, strings.ToUpper(x.Request.Method)+" "+path)
		}
	}
	return reqs
}

var pathParam = regexp.MustCompile(`\\\{[^}]+\\\}`)

func assertFixturesCover(t *testing.T, patterns map[string]bool, reqs []string) {
	t.Helper()
	for p := range patterns {
		re := regexp.MustCompile("^" + pathParam.ReplaceAllString(regexp.QuoteMeta(p), `[^/]+`) + "$")
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

func TestEveryAutomationRouteHasFixture(t *testing.T) {
	autos, _ := automationGoPatterns(t)
	if len(autos) != 11 {
		t.Fatalf("want 11 automations routes, got %d", len(autos))
	}
	assertFixturesCover(t, autos, fixtureRequests(t, "automations"))
}

func TestEveryWatchRouteHasFixture(t *testing.T) {
	_, watches := automationGoPatterns(t)
	if len(watches) != 7 {
		t.Fatalf("want 7 watches routes, got %d", len(watches))
	}
	assertFixturesCover(t, watches, fixtureRequests(t, "watches"))
}

// TestRetainedAutomationListMatchesFastAPI: the diff of the FastAPI route table against the Go
// patterns plus the retained list is empty (and the retained list is explicitly empty).
func TestRetainedAutomationListMatchesFastAPI(t *testing.T) {
	routes := pythonAutomationRoutes(t)
	autos, watches := automationGoPatterns(t)
	retained := readRetainedAutomations(t)
	ported := map[string]bool{}
	for p := range autos {
		ported[p] = true
	}
	for p := range watches {
		ported[p] = true
	}
	if len(ported) != 18 || len(retained) != 0 {
		t.Errorf("want 18 ported + 0 retained routes, got %d + %d", len(ported), len(retained))
	}
	if len(routes) != 18 {
		t.Errorf("want 18 Python route decorators, parsed %d", len(routes))
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

// TestPlan08DeviationsDocumented: D4-D7 and the carried documentation items are in README.md.
func TestPlan08DeviationsDocumented(t *testing.T) {
	b, err := os.ReadFile(contractREADME)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	i := strings.Index(doc, "## Plan 08 coverage and rollout (Task 21): deviations D4-D7")
	if i < 0 {
		t.Fatal("README lacks the plan 08 Task 21 section")
	}
	sec := doc[i:]
	if j := strings.Index(sec[3:], "\n## "); j >= 0 {
		sec = sec[:j+3]
	}
	for _, want := range []string{"D4", "D5", "D6", "D7", "retained_automation_routes.txt", "json.Number", "pending watch_poll", "dry run",
		"per-rule skip", "TZ", "CREDENTIALS_SOURCE=local", "mirror delete", "poll-now"} {
		if !strings.Contains(sec, want) {
			t.Errorf("plan 08 README section does not mention %q", want)
		}
	}
}
