package api

import (
	"io"
	"log/slog"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// connectorPatterns are exactly the 19 routes Tasks 15-18 register. Stubs
// register nothing until their task lands, so the registered set must always
// be a subset of this table.
var connectorPatterns = map[string]bool{
	"GET /api/connectors/catalog":                         true,
	"GET /api/connectors/compatibility":                   true,
	"GET /api/crm/hubspot/status":                         true,
	"POST /api/crm/hubspot/connect":                       true,
	"POST /api/crm/hubspot/sync":                          true,
	"GET /api/crm/hubspot/contacts":                       true,
	"GET /api/data":                                       true,
	"POST /api/data/mark-used":                            true,
	"PATCH /api/data/{id}":                                true,
	"DELETE /api/data/{id}":                               true,
	"GET /api/data/export":                                true,
	"GET /api/data/stats/timeline":                        true,
	"GET /api/data/stats/domains":                         true,
	"GET /api/ambitionbox/companies":                      true,
	"GET /api/ambitionbox/companies/{company_id}/jobs":    true,
	"GET /api/ambitionbox/companies/{company_id}/detail":  true,
	"GET /api/ambitionbox/companies/{company_id}/similar": true,
	"GET /api/ambitionbox/collect":                        true,
	"POST /api/ambitionbox/imports":                       true,
}

func connectorDeps(pg, on bool) Deps {
	return Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Flags: config.Flags{PGLeadStore: pg, GoConnectorRoutesEnabled: on}}
}

func registerConnectorGroups(r *httpx.Router, d Deps) {
	RegisterConnectors(r, d)
	RegisterHubspot(r, d)
	RegisterDataCRM(r, d)
	RegisterAmbitionBox(r, d)
}

func TestConnectorRoutesNotRegisteredWithoutFlags(t *testing.T) {
	for _, c := range []struct{ pg, on bool }{{false, false}, {true, false}, {false, true}} {
		r := httpx.NewRouter()
		registerConnectorGroups(r, connectorDeps(c.pg, c.on))
		RegisterAll(httpx.NewRouter(), connectorDeps(c.pg, c.on))
		if n := len(r.Patterns()); n != 0 {
			t.Errorf("pg=%v on=%v: %d routes registered: %v", c.pg, c.on, n, r.Patterns())
		}
	}
	if connectorDeps(true, false).connectorRoutesEnabled("x") || connectorDeps(false, true).connectorRoutesEnabled("x") {
		t.Fatal("connectorRoutesEnabled needs both flags")
	}
	if !connectorDeps(true, true).connectorRoutesEnabled("x") {
		t.Fatal("connectorRoutesEnabled false with both flags")
	}
}

func TestConnectorRoutePatterns(t *testing.T) {
	if len(connectorPatterns) != 19 {
		t.Fatalf("pattern table has %d entries, want 19", len(connectorPatterns))
	}
	r := httpx.NewRouter()
	registerConnectorGroups(r, connectorDeps(true, true))
	for _, p := range r.Patterns() {
		if !connectorPatterns[p] {
			t.Errorf("unexpected pattern %s", p)
		}
	}
}
