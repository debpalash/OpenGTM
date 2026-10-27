package api

import (
	"bufio"
	"context"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testutil"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

const retainedLeadsFile = "../../testdata/contract/retained_leads_routes.txt"

func readRetainedLeads(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open(retainedLeadsFile)
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

func leadsGoPatterns(t *testing.T) map[string]bool {
	// pgxpool.New is lazy: no connection is made, the pool only has to be non-nil.
	pool, err := pgxpool.New(context.Background(), "postgres://coverage@127.0.0.1:1/coverage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	d := Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Flags: config.Flags{PGLeadStore: true}, Pool: pool}
	r := httpx.NewRouter()
	RegisterLeads(r, d)
	RegisterSignals(r, d)
	RegisterEvents(r, d)
	out := map[string]bool{}
	for _, p := range r.Patterns() {
		if !goOnlyEventsPatterns[p] {
			out[p] = true
		}
	}
	return out
}

var (
	pyRouterDecl = regexp.MustCompile(`(?m)^(\w+)\s*=\s*APIRouter\(([^)]*)\)`)
	pyPrefix     = regexp.MustCompile(`prefix\s*=\s*"([^"]*)"`)
	pyRoute      = regexp.MustCompile(`(?m)^@(\w+)\.(get|post|put|delete|patch)\(\s*"([^"]*)"`)
)

// pythonLeadSignalRoutes parses every route decorator of leads.py and signals.py.
func pythonLeadSignalRoutes(t *testing.T) []string {
	t.Helper()
	var routes []string
	for _, name := range []string{"leads.py", "signals.py"} {
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
		for _, m := range pyRoute.FindAllStringSubmatch(src, -1) {
			pre, ok := prefixes[m[1]]
			if !ok {
				t.Fatalf("%s: decorator on unknown router %q", name, m[1])
			}
			routes = append(routes, strings.ToUpper(m[2])+" "+pre+m[3])
		}
	}
	return routes
}

func TestEveryPythonLeadSignalRouteIsPortedOrRetained(t *testing.T) {
	routes := pythonLeadSignalRoutes(t)
	ported, retained := leadsGoPatterns(t), readRetainedLeads(t)
	if len(ported) != 20 || len(retained) != 7 {
		t.Errorf("want 20 ported + 7 retained, got %d + %d", len(ported), len(retained))
	}
	// 14 routes of leads.py (collect, jobs*, system-stats, lead utilities) moved to Go in plan 14.
	lg := map[string]bool{}
	for p := range leadgenGoPatterns(t) {
		lg[normRoute(p)] = true
	}
	ported14 := 0
	for _, r := range routes {
		if !ported[r] && !retained[r] && lg[normRoute(r)] {
			ported[r] = true
			ported14++
		}
	}
	if ported14 != 14 {
		t.Errorf("want 14 leads.py routes ported by plan 14, got %d", ported14)
	}
	if len(routes) != 41 {
		t.Errorf("want 41 Python route decorators, parsed %d", len(routes))
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

func TestRetainedRoutesNotRegisteredInGo(t *testing.T) {
	ported := leadsGoPatterns(t)
	for r := range readRetainedLeads(t) {
		if ported[r] {
			t.Errorf("retained route registered in Go: %s", r)
		}
	}
}
