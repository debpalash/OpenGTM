package api

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"

// RegisterLeads registers the 14 lead routes. Go supports only the Postgres
// lead store, so nothing registers when PG_LEAD_STORE is false.
func RegisterLeads(r *httpx.Router, d Deps) {
	if !d.workbooksEnabled("leads") {
		return
	}
	registerReads(r, d)
	registerWrites(r, d)
	registerTimeline(r, d)
	registerExport(r, d)
	registerDedup(r, d)
	registerBulk(r, d)
}

// RegisterSignals registers the signal routes (not /api/signals/sources).
func RegisterSignals(r *httpx.Router, d Deps) {
	if !d.workbooksEnabled("signals") {
		return
	}
	registerSignals(r, d)
}

// RegisterEvents registers GET /api/events (SSE).
func RegisterEvents(r *httpx.Router, d Deps) {
	if !d.workbooksEnabled("events") {
		return
	}
	registerEvents(r, d)
}
