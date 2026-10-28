package api

import (
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// leadgenRoutesEnabled reports whether the leadgen routes may register: GO_COLLECTION_ROUTES_ENABLED (retired:
// always on) and PG_LEAD_STORE. Go always uses the Postgres collection ledger (final correctness I3): the
// __collection_ledger_backend marker is not consulted, so a missing, odd or unreadable marker cannot remove the surface.
func (d Deps) leadgenRoutesEnabled(area string) bool {
	if !d.Flags.Leadgen.GoCollectionRoutesEnabled {
		d.logger().Warn("leadgen routes not registered: GO_COLLECTION_ROUTES_ENABLED is false", "area", area)
		return false
	}
	if !d.Flags.PGLeadStore {
		d.logger().Warn("leadgen routes not registered: PG_LEAD_STORE is false (Go supports the Postgres lead store only)", "area", area)
		return false
	}
	return true
}

// RegisterCollect registers POST /api/collect (Task 14).
func RegisterCollect(r *httpx.Router, d Deps) {
	if !d.leadgenRoutesEnabled("collect") {
		return
	}
	registerCollectRoute(r, d)
}

// RegisterJobs registers the 8 job routes and /api/system-stats (Tasks 15-17, 21).
func RegisterJobs(r *httpx.Router, d Deps) {
	if !d.leadgenRoutesEnabled("jobs") {
		return
	}
	registerJobsRead(r, d)
	registerJobsWrite(r, d)
	registerSystemStats(r, d)
}

// RegisterLeadUtils registers the 4 /api/leads/{score,verify-email,tech-stack,domain-intel} routes (Tasks 18-19).
func RegisterLeadUtils(r *httpx.Router, d Deps) {
	if !d.leadgenRoutesEnabled("leadutils") {
		return
	}
	registerLeadUtils(r, d)
	registerDomainIntel(r, d)
}

// RegisterSourceSettings registers the 5 /api/settings/sources* routes (Task 20). Literal paths
// are registered with their own methods; net/http prefers them over {source_id}.
func RegisterSourceSettings(r *httpx.Router, d Deps) {
	if !d.leadgenRoutesEnabled("sourcesettings") {
		return
	}
	registerSourceSettings(r, d)
}
