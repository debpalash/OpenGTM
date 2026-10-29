package api

import (
	"io"
	"log/slog"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// Generated from legacy/python/apps/api/routers/governance.py (21 routes; /capabilities stays on
// FastAPI per D1, so 17 are ported) and playbooks.py (10 routes; /capabilities retained, 9 ported).
var governancePatterns = map[string]bool{
	"GET /api/governance/audit-events":                true,
	"GET /api/governance/audit-events/export.csv":     true,
	"GET /api/governance/retention":                   true,
	"PUT /api/governance/retention":                   true,
	"POST /api/governance/retention/preview":          true,
	"POST /api/governance/retention/enforce":          true,
	"GET /api/governance/retention/runs":              true,
	"GET /api/governance/rbac":                        true,
	"PUT /api/governance/rbac/{user_id}/{permission}": true,
	"POST /api/governance/members":                    true,
	"PATCH /api/governance/members/{user_id}":         true,
	"DELETE /api/governance/members/{user_id}":        true,
	"GET /api/governance/sso":                         true,
	"PUT /api/governance/sso":                         true,
	"GET /api/governance/scim-token":                  true,
	"POST /api/governance/scim-token":                 true,
	"DELETE /api/governance/scim-token":               true,
}

var playbookPatterns = map[string]bool{
	"GET /api/research-playbooks":                       true,
	"POST /api/research-playbooks":                      true,
	"DELETE /api/research-playbooks/{playbook_id}":      true,
	"PATCH /api/research-playbooks/{playbook_id}":       true,
	"POST /api/research-playbooks/{playbook_id}/runs":   true,
	"GET /api/research-playbooks/{playbook_id}/runs":    true,
	"GET /api/research-playbooks/runs/{run_id}/results": true,
	"POST /api/research-playbooks/runs/{run_id}/retry":  true,
	"POST /api/research-playbooks/runs/{run_id}/cancel": true,
}

func govDeps(pg, gov, pb bool) Deps {
	return Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Flags: config.Flags{PGLeadStore: pg, Governance: config.GovernanceSettings{GoGovernanceRoutesEnabled: gov, GoPlaybookRoutesEnabled: pb}}}
}

func TestGovernanceRoutesNotRegisteredWithoutFlag(t *testing.T) {
	for _, c := range []struct{ pg, on bool }{{false, false}, {true, false}, {false, true}} {
		r := httpx.NewRouter()
		RegisterGovernance(r, govDeps(c.pg, c.on, c.on))
		RegisterPlaybooks(r, govDeps(c.pg, c.on, c.on))
		if n := len(r.Patterns()); n != 0 {
			t.Errorf("pg=%v on=%v: %d routes registered: %v", c.pg, c.on, n, r.Patterns())
		}
	}
	RegisterAll(httpx.NewRouter(), govDeps(true, false, false)) // must not panic
}

func TestGovernanceRoutePatterns(t *testing.T) {
	if len(governancePatterns) != 17 {
		t.Fatalf("pattern table has %d entries, want 17", len(governancePatterns))
	}
	r := httpx.NewRouter()
	RegisterGovernance(r, govDeps(true, true, true))
	for _, p := range r.Patterns() { // stubs register nothing until Tasks 12-15; must stay a subset
		if !governancePatterns[p] {
			t.Errorf("unexpected governance pattern %s", p)
		}
	}
}

func TestPlaybookRoutePatterns(t *testing.T) {
	if len(playbookPatterns) != 9 {
		t.Fatalf("pattern table has %d entries, want 9", len(playbookPatterns))
	}
	r := httpx.NewRouter()
	RegisterPlaybooks(r, govDeps(true, true, true))
	for _, p := range r.Patterns() {
		if !playbookPatterns[p] {
			t.Errorf("unexpected playbook pattern %s", p)
		}
	}
}
