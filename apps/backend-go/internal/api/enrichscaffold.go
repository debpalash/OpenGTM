package api

import (
	"log/slog"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// runRoutesEnabled reports whether the enrichment route groups may register:
// both PG_LEAD_STORE and GO_RUN_ROUTES_ENABLED must be on. Otherwise nothing
// registers and the reason is logged, so a canary cannot enable them by mistake.
func (d Deps) runRoutesEnabled(area string) bool {
	if d.Flags.PGLeadStore && d.Flags.GoRunRoutesEnabled {
		return true
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	log.Warn("enrichment routes not registered: PG_LEAD_STORE and GO_RUN_ROUTES_ENABLED must both be true",
		"area", area, "pg_lead_store", d.Flags.PGLeadStore, "go_run_routes_enabled", d.Flags.GoRunRoutesEnabled)
	return false
}

// RegisterWorkbookRuns registers estimate, stop, provider-stats, run, cell run,
// sources, refresh, refresh-policy and migrate (Tasks 23-28) and
// GET /api/signals/sources (Task 17, gated with the run routes).
func RegisterWorkbookRuns(r *httpx.Router, d Deps) {
	if !d.runRoutesEnabled("workbook-runs") {
		return
	}
	registerWorkbookEstimate(r, d)
	registerWorkbookRun(r, d)
	registerWorkbookCell(r, d)
	registerWorkbookSources(r, d)
	registerWorkbookRefresh(r, d)
	registerWorkbookMigrate(r, d)
	registerSignalSources(r, d)
}

// RegisterIngest registers the ingest-token and rows/ingest routes (Task 30).
func RegisterIngest(r *httpx.Router, d Deps) {
	if !d.runRoutesEnabled("ingest") {
		return
	}
	registerIngest(r, d)
}

// RegisterWorkbookWS registers the workbook WebSocket (Task 29).
func RegisterWorkbookWS(r *httpx.Router, d Deps) {
	if !d.runRoutesEnabled("workbook-ws") {
		return
	}
	registerWorkbookWS(r, d)
}
