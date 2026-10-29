package api

import (
	"context"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testutil"
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
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/playbooks"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/retention"
)

// Plan 11 coverage (Task 19): 28 FastAPI routes (18 governance + 10 research-playbooks), 26 ported, 2 retained
// (the two /capabilities routes, D1); every ported route has a recorded fixture.

const retainedGovernanceFile = "../../testdata/contract/retained_governance_routes.txt"

func governanceGoPatterns(t *testing.T) map[string]bool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://coverage@127.0.0.1:1/coverage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	d := Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Pool: pool, Identity: identity.NewStore(pool),
		Getenv: func(string) string { return "" },
		Flags: config.Flags{PGLeadStore: true, Governance: config.GovernanceSettings{
			GoGovernanceRoutesEnabled: true, GoPlaybookRoutesEnabled: true}}}
	out := map[string]bool{}
	for _, register := range []func(*httpx.Router, Deps){RegisterGovernance, RegisterPlaybooks} {
		r := httpx.NewRouter()
		register(r, d)
		for _, p := range r.Patterns() {
			out[p] = true
		}
	}
	// Plan 15 WP-ROUTES-A: the two capability catalogs moved from the retained list into Go.
	for p := range capabilityPatterns(t, "/api/governance/", "/api/research-playbooks/") {
		out[p] = true
	}
	return out
}

func readRetainedGovernance(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(retainedGovernanceFile)
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

func legacyAPIFile(t *testing.T, rel ...string) string {
	t.Helper()
	p := testutil.LegacyPath(t, append([]string{"apps", "api"}, rel...)...)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("legacy Python tree not present (%v)", err)
	}
	return string(b)
}

// pythonGovernanceRoutes parses routers/governance.py and routers/playbooks.py (prefix + decorators).
func pythonGovernanceRoutes(t *testing.T) (gov, pb []string) {
	t.Helper()
	parse := func(name string) []string {
		src := legacyAPIFile(t, "routers", name)
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
		sort.Strings(routes)
		return routes
	}
	return parse("governance.py"), parse("playbooks.py")
}

func TestEveryGovernancePlaybookRouteIsPortedOrRetained(t *testing.T) {
	gov, pb := pythonGovernanceRoutes(t)
	if len(gov) != 18 || len(pb) != 10 {
		t.Fatalf("want 18 governance + 10 playbook Python routes, parsed %d + %d: %v %v", len(gov), len(pb), gov, pb)
	}
	ported := governanceGoPatterns(t)
	retained := readRetainedGovernance(t)
	if len(ported) != 28 || len(retained) != 0 {
		t.Errorf("want 28 ported (26 + the two capability catalogs, plan 15) + 0 retained, got %d + %d", len(ported), len(retained))
	}
	seen := map[string]bool{}
	for _, r := range append(append([]string{}, gov...), pb...) {
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

func TestRetainedGovernanceRoutesNotRegisteredInGo(t *testing.T) {
	ported := governanceGoPatterns(t)
	retained := readRetainedGovernance(t)
	for _, want := range []string{"GET /api/governance/capabilities", "GET /api/research-playbooks/capabilities"} {
		if retained[want] || !ported[want] {
			t.Errorf("%s must be ported (plan 15 WP-ROUTES-A), not retained", want)
		}
	}
	for r := range retained {
		if ported[r] {
			t.Errorf("retained route is registered in Go: %s", r)
		}
	}
}

func TestEveryGovernancePlaybookRouteHasFixtureOrIsRetained(t *testing.T) {
	ported := governanceGoPatterns(t)
	retained := readRetainedGovernance(t)
	rest := map[string]bool{}
	for p := range ported {
		// The capability catalogs are recorded under testdata/contract/capabilities (capabilityroutes_test.go).
		if !retained[p] && !strings.HasSuffix(p, "/capabilities") {
			rest[p] = true
		}
	}
	assertFixturesCover(t, rest, fixtureRequests(t, "governance", "retention", "rbac", "sso", "playbooks"))
}

// The three ported job types are exactly the Python ones with a GO_EXECUTOR_* flag; since the plan 15 cutover the
// flags are retired and Go claims them for every workspace.
func TestGovernanceJobTypesAlwaysRouteToGo(t *testing.T) {
	timeouts := legacyAPIFile(t, "services", "queue_service.py")
	flags := legacyAPIFile(t, "services", "go_executor.py")
	for typ, flag := range map[string]string{
		retention.JobType:         "GO_EXECUTOR_RETENTION",
		playbooks.RunJobType:      "GO_EXECUTOR_PLAYBOOKS",
		playbooks.ScheduleJobType: "GO_EXECUTOR_PLAYBOOKS",
	} {
		if !strings.Contains(timeouts, `"`+typ+`"`) {
			t.Errorf("%s is not a Python job type", typ)
		}
		if !regexp.MustCompile(`"` + typ + `":\s*"` + flag + `"`).MatchString(flags) {
			t.Errorf("Python go_executor does not map %s to %s", typ, flag)
		}
	}
	ex := config.ExecutorSettings{Enabled: true, Workspaces: map[string]bool{"w1": true}}
	for _, g := range []config.GovernanceSettings{{}, {GoExecutorRetention: true, GoExecutorPlaybooks: true}} {
		for _, e := range []config.ExecutorSettings{ex, {}} {
			for _, ws := range []string{"w1", "w2", ""} {
				if got := playbooks.ExecutorFor(e, g, ws); got != "go" {
					t.Errorf("playbooks ws=%q flags %+v: executor %q, want go", ws, g, got)
				}
				if got := retention.ExecutorFor(e, g, ws); got != "go" {
					t.Errorf("retention ws=%q flags %+v: executor %q, want go", ws, g, got)
				}
			}
		}
	}
}

var (
	pyTargetRow  = regexp.MustCompile(`\("(\w+)",\s*(\w+),\s*\w+\.(\w+),\s*"(\w+)"\)`)
	pyClassTable = regexp.MustCompile(`(?m)^class (\w+)\([^)]*\):[^\n]*\n(?:[ \t]+[^\n]*\n|\n)*?[ \t]+__tablename__\s*=\s*"(\w+)"`)
)

func TestRetentionTargetsCoverEveryLegacyOrmModel(t *testing.T) {
	src := legacyAPIFile(t, "services", "governance", "retention.py")
	start := strings.Index(src, "models = [")
	end := strings.Index(src[start:], "]\n")
	rows := pyTargetRow.FindAllStringSubmatch(src[start:start+end], -1)
	if len(rows) != 8 {
		t.Fatalf("want 8 Python _targets rows, parsed %d", len(rows))
	}
	tables := map[string]string{}
	filepath.WalkDir(testutil.LegacyPath(t, "apps", "api", "services"),
		func(p string, de os.DirEntry, err error) error {
			if err != nil || de.IsDir() || !strings.HasSuffix(p, ".py") || strings.HasSuffix(p, "_test.py") {
				return nil
			}
			if b, e := os.ReadFile(p); e == nil {
				for _, m := range pyClassTable.FindAllStringSubmatch(string(b), -1) {
					tables[m[1]] = m[2]
				}
			}
			return nil
		})
	kinds := map[string]retention.CutoffKind{"datetime": retention.KindTimestamp, "date": retention.KindDate, "epoch": retention.KindEpoch}
	goTargets := retention.Targets()
	if len(goTargets) != len(rows) {
		t.Fatalf("Go has %d retention targets, Python %d", len(goTargets), len(rows))
	}
	for i, r := range rows {
		table, ok := tables[r[2]]
		if !ok {
			t.Fatalf("no __tablename__ found for Python model %s", r[2])
		}
		g := goTargets[i]
		if g.Category != r[1] || g.Table != table || g.Kind != kinds[r[4]] {
			t.Errorf("target %d: Go {%s %s %v}, Python {%s %s %s}", i, g.Category, g.Table, g.Kind, r[1], table, r[4])
		}
	}
}

// RegisterPlaybooks wires playbooks.ScheduleExecutor (the PATCH-enqueued schedule job's executor); since the plan 15
// cutover it is go for every workspace, whatever the retired cohort flags say.
func TestPlaybookScheduleExecutorWiredByRegister(t *testing.T) {
	old := playbooks.ScheduleExecutor
	t.Cleanup(func() { playbooks.ScheduleExecutor = old })
	playbooks.ScheduleExecutor = func(string) string { return "unwired" }
	pool, err := pgxpool.New(context.Background(), "postgres://coverage@127.0.0.1:1/coverage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	env := map[string]string{"GO_EXECUTOR_ENABLED": "true", "GO_EXECUTOR_WORKSPACES": "w1"}
	d := Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Pool: pool, Identity: identity.NewStore(pool),
		Getenv: func(k string) string { return env[k] },
		Flags: config.Flags{PGLeadStore: true, Governance: config.GovernanceSettings{
			GoPlaybookRoutesEnabled: true, GoExecutorPlaybooks: true}}}
	RegisterPlaybooks(httpx.NewRouter(), d)
	if got := playbooks.ScheduleExecutor("w1"); got != "go" {
		t.Errorf("cohort workspace schedule executor %q, want go", got)
	}
	if got := playbooks.ScheduleExecutor("w2"); got != "go" {
		t.Errorf("other workspace schedule executor %q, want go", got)
	}
	env["GO_EXECUTOR_ENABLED"] = ""
	if got := playbooks.ScheduleExecutor("w1"); got != "go" {
		t.Errorf("cohort off: schedule executor %q, want go", got)
	}
}

// The deviations the runbook and README promise operators must be written down.
func TestPlan11DeviationsDocumented(t *testing.T) {
	b, err := os.ReadFile("../../testdata/contract/README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Plan 11 deviations", "D-rbac-1", "legal hold", "per cell per attempt", "secret before the config",
		"503", "formula", "SCIM removeMember", "retained_governance_routes.txt"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("README missing plan 11 deviation text %q", want)
		}
	}
	rb, err := os.ReadFile("../../../../docs/go-port/runbooks/governance-playbooks-cutover.md")
	if err != nil {
		t.Fatalf("runbook missing: %v", err)
	}
	for _, want := range []string{"GO_GOVERNANCE_ROUTES_ENABLED", "GO_PLAYBOOK_ROUTES_ENABLED", "GO_EXECUTOR_RETENTION",
		"GO_EXECUTOR_PLAYBOOKS", "PY_WORKER_URL", "dry-run", "Rollback", "reaper"} {
		if !strings.Contains(string(rb), want) {
			t.Errorf("runbook missing %q", want)
		}
	}
}
