package api

import (
	"io"
	"log/slog"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// automationPatterns / watchPatterns are exactly the 11 and 7 routes Tasks 17-20
// register. Stubs register nothing until their task lands, so the registered set
// must always be a subset of these tables.
var automationPatterns = map[string]bool{
	"POST /api/automations/triggers":                      true,
	"GET /api/automations/triggers":                       true,
	"GET /api/automations/triggers/{trigger_id}":          true,
	"PATCH /api/automations/triggers/{trigger_id}":        true,
	"DELETE /api/automations/triggers/{trigger_id}":       true,
	"POST /api/automations/triggers/{trigger_id}/pause":   true,
	"POST /api/automations/triggers/{trigger_id}/resume":  true,
	"POST /api/automations/triggers/{trigger_id}/preview": true,
	"POST /api/automations/triggers/{trigger_id}/run":     true,
	"GET /api/automations/triggers/{trigger_id}/runs":     true,
	"GET /api/automations/runs/{run_id}":                  true,
}

var watchPatterns = map[string]bool{
	"POST /api/watches":                   true,
	"GET /api/watches":                    true,
	"GET /api/watches/{watch_id}":         true,
	"PATCH /api/watches/{watch_id}":       true,
	"DELETE /api/watches/{watch_id}":      true,
	"POST /api/watches/{watch_id}/poll":   true,
	"GET /api/watches/{watch_id}/signals": true,
}

func autoDeps(pg, on, legacy bool) Deps {
	return Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Flags: config.Flags{PGLeadStore: pg, AutomationsAllowLegacyOutreach: legacy,
			Automations: config.AutomationsSettings{GoRoutesEnabled: on}}}
}

func registerAutomationGroups(r *httpx.Router, d Deps) {
	RegisterAutomations(r, d)
	RegisterWatches(r, d)
}

func TestAutomationRoutesNotRegisteredWithoutFlags(t *testing.T) {
	for _, c := range []struct{ pg, on bool }{{false, false}, {true, false}, {false, true}} {
		r := httpx.NewRouter()
		registerAutomationGroups(r, autoDeps(c.pg, c.on, false))
		for _, p := range r.Patterns() {
			// The rule routes need only GO_AUTOMATION_ROUTES_ENABLED (Python gates them on AUTOMATIONS_ENABLED
			// alone); the watch routes also need PG_LEAD_STORE.
			if !c.on || watchPatterns[p] {
				t.Errorf("pg=%v on=%v: route %s registered", c.pg, c.on, p)
			}
		}
	}
	if autoDeps(true, false, false).automationRoutesEnabled("x") || !autoDeps(true, true, false).automationRoutesEnabled("x") {
		t.Fatal("automationRoutesEnabled must follow GO_AUTOMATION_ROUTES_ENABLED")
	}
	if autoDeps(false, true, false).watchRoutesEnabled("x") || autoDeps(true, false, false).watchRoutesEnabled("x") ||
		!autoDeps(true, true, false).watchRoutesEnabled("x") {
		t.Fatal("watchRoutesEnabled needs GO_AUTOMATION_ROUTES_ENABLED and PG_LEAD_STORE")
	}
	RegisterAll(httpx.NewRouter(), autoDeps(true, false, false)) // must not panic
}

func TestAutomationRoutesRefusedWithLegacyOutreach(t *testing.T) {
	d := autoDeps(true, true, true)
	if d.automationRoutesEnabled("x") {
		t.Fatal("automation routes must refuse when AUTOMATIONS_ALLOW_LEGACY_OUTREACH is true (D2)")
	}
	r := httpx.NewRouter()
	RegisterAutomations(r, d)
	if n := len(r.Patterns()); n != 0 {
		t.Fatalf("%d automation routes registered with legacy outreach: %v", n, r.Patterns())
	}
	if !d.watchRoutesEnabled("x") {
		t.Fatal("watch routes do not depend on legacy outreach")
	}
}

func TestAutomationRoutePatterns(t *testing.T) {
	if len(automationPatterns) != 11 || len(watchPatterns) != 7 {
		t.Fatalf("pattern tables have %d and %d entries, want 11 and 7", len(automationPatterns), len(watchPatterns))
	}
	r := httpx.NewRouter()
	RegisterAutomations(r, autoDeps(true, true, false))
	for _, p := range r.Patterns() {
		if !automationPatterns[p] {
			t.Errorf("unexpected automation pattern %s", p)
		}
	}
	r = httpx.NewRouter()
	RegisterWatches(r, autoDeps(true, true, false))
	for _, p := range r.Patterns() {
		if !watchPatterns[p] {
			t.Errorf("unexpected watch pattern %s", p)
		}
	}
}
