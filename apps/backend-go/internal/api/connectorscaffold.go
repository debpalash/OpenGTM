package api

import (
	"log/slog"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// connectorRoutesEnabled reports whether the connector/CRM route groups may
// register: both PG_LEAD_STORE and GO_CONNECTOR_ROUTES_ENABLED must be on.
// Otherwise nothing registers and the reason is logged.
func (d Deps) connectorRoutesEnabled(area string) bool {
	if d.Flags.PGLeadStore && d.Flags.GoConnectorRoutesEnabled {
		return true
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	log.Warn("connector routes not registered: PG_LEAD_STORE and GO_CONNECTOR_ROUTES_ENABLED must both be true",
		"area", area, "pg_lead_store", d.Flags.PGLeadStore, "go_connector_routes_enabled", d.Flags.GoConnectorRoutesEnabled)
	return false
}

// RegisterConnectors registers GET /api/connectors/catalog and /compatibility (Task 15).
func RegisterConnectors(r *httpx.Router, d Deps) {
	if d.connectorRoutesEnabled("connectors") {
		registerConnectors(r, d)
	}
}

// RegisterHubspot registers the /api/crm/hubspot routes (Task 16).
func RegisterHubspot(r *httpx.Router, d Deps) {
	if d.connectorRoutesEnabled("hubspot") {
		registerHubspot(r, d)
	}
}

// RegisterDataCRM registers the 7 /api/data routes (Task 17).
func RegisterDataCRM(r *httpx.Router, d Deps) {
	if d.connectorRoutesEnabled("datacrm") {
		registerDataCRM(r, d)
	}
}

// RegisterAmbitionBox registers the /api/ambitionbox routes (Task 18).
func RegisterAmbitionBox(r *httpx.Router, d Deps) {
	if d.connectorRoutesEnabled("ambitionbox") {
		registerAmbitionBox(r, d)
	}
}
