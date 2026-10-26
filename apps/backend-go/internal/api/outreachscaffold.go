package api

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"

// outreachRoutesEnabled reports whether the outreach routes may register:
// GO_OUTREACH_ROUTES_ENABLED and PG_LEAD_STORE must both be on. Otherwise nothing
// registers and the reason is logged, so an nginx canary cannot be enabled by mistake.
func (d Deps) outreachRoutesEnabled(area string) bool {
	if !d.Flags.Outreach.GoRoutesEnabled {
		d.logger().Warn("outreach routes not registered: GO_OUTREACH_ROUTES_ENABLED is false", "area", area)
		return false
	}
	if !d.Flags.PGLeadStore {
		d.logger().Warn("outreach routes not registered: PG_LEAD_STORE is false (Go supports the Postgres lead store only)", "area", area)
		return false
	}
	return true
}

// RegisterOutreach registers the 22 /api/outreach routes (Tasks 20-25).
func RegisterOutreach(r *httpx.Router, d Deps) {
	if !d.outreachRoutesEnabled("outreach") {
		return
	}
	registerOutreachSequences(r, d)
	registerOutreachActions(r, d)
	registerOutreachSMTP(r, d)
	registerOutreachSuppressions(r, d)
	registerOutreachDrafts(r, d)
	registerOutreachPublic(r, d)
}
