package api

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"

// audienceRoutesEnabled: GO_AUDIENCE_ROUTES_ENABLED and PG_LEAD_STORE on, and the membership
// event emitter wired (D5). Otherwise nothing registers and the reason is logged.
func (d Deps) audienceRoutesEnabled(area string) bool {
	if !d.Flags.Audiences.GoAudienceRoutesEnabled {
		d.logger().Warn("audience routes not registered: GO_AUDIENCE_ROUTES_ENABLED is false", "area", area)
		return false
	}
	if !d.Flags.PGLeadStore {
		d.logger().Warn("audience routes not registered: PG_LEAD_STORE is false (Go supports the Postgres lead store only)", "area", area)
		return false
	}
	if d.AudienceEvents == nil {
		d.logger().Warn("audience routes not registered: membership event emitter not wired", "area", area)
		return false
	}
	return true
}

// destRoutesEnabled: GO_DEST_ROUTES_ENABLED and PG_LEAD_STORE on.
func (d Deps) destRoutesEnabled(area string) bool {
	if !d.Flags.Audiences.GoDestRoutesEnabled {
		d.logger().Warn("destination routes not registered: GO_DEST_ROUTES_ENABLED is false", "area", area)
		return false
	}
	if !d.Flags.PGLeadStore {
		d.logger().Warn("destination routes not registered: PG_LEAD_STORE is false (Go supports the Postgres lead store only)", "area", area)
		return false
	}
	return true
}

// RegisterAudiences registers the 8 /api/audiences routes (Tasks 15-17).
func RegisterAudiences(r *httpx.Router, d Deps) {
	if !d.audienceRoutesEnabled("audiences") {
		return
	}
	registerAudiencesCrud(r, d)
	registerAudiencesRead(r, d)
	registerAudiencesAccounts(r, d)
}

// RegisterDestinations registers the 15 /api/audience-destinations routes (Tasks 18-21).
func RegisterDestinations(r *httpx.Router, d Deps) {
	if !d.destRoutesEnabled("destinations") {
		return
	}
	registerDestsCrud(r, d)
	registerDestsRuns(r, d)
	registerDestsInboundTokens(r, d)
	registerDestsInbound(r, d)
}
