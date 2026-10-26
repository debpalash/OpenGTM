package api

import (
	"log/slog"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

func (d Deps) logger() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// automationRoutesEnabled reports whether the automations routes may register:
// GO_AUTOMATION_ROUTES_ENABLED must be on and AUTOMATIONS_ALLOW_LEGACY_OUTREACH off
// (D2: legacy outreach actions stay on Python, which keeps the whole surface).
// Otherwise nothing registers and the reason is logged.
func (d Deps) automationRoutesEnabled(area string) bool {
	if !d.Flags.Automations.GoRoutesEnabled {
		d.logger().Warn("automation routes not registered: GO_AUTOMATION_ROUTES_ENABLED is false", "area", area)
		return false
	}
	if d.Flags.AutomationsAllowLegacyOutreach {
		d.logger().Warn("automation routes not registered: AUTOMATIONS_ALLOW_LEGACY_OUTREACH is true (legacy outreach actions stay on Python)", "area", area)
		return false
	}
	return true
}

// watchRoutesEnabled reports whether the watches routes may register:
// GO_AUTOMATION_ROUTES_ENABLED and PG_LEAD_STORE must both be on.
func (d Deps) watchRoutesEnabled(area string) bool {
	if !d.Flags.Automations.GoRoutesEnabled {
		d.logger().Warn("watch routes not registered: GO_AUTOMATION_ROUTES_ENABLED is false", "area", area)
		return false
	}
	if !d.Flags.PGLeadStore {
		d.logger().Warn("watch routes not registered: PG_LEAD_STORE is false (Go supports the Postgres lead store only)", "area", area)
		return false
	}
	return true
}

// RegisterAutomations registers the 11 /api/automations routes (Tasks 17-18).
func RegisterAutomations(r *httpx.Router, d Deps) {
	if d.automationRoutesEnabled("automations") {
		registerAutomationsCRUD(r, d)
		registerAutomationsRun(r, d)
	}
}

// RegisterWatches registers the 7 /api/watches routes (Tasks 19-20).
func RegisterWatches(r *httpx.Router, d Deps) {
	if d.watchRoutesEnabled("watches") {
		registerWatchesCRUD(r, d)
		registerWatchesPoll(r, d)
	}
}
