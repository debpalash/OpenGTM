package api

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// collectionLedgerMarkerKey is the app_settings row written last by
// legacy/python/scripts/migrate_collection_ledger.py cutover.
const collectionLedgerMarkerKey = "__collection_ledger_backend"

// LoadCollectionLedgerBackend reads the cutover marker ("" when absent). cmd/api stores the result
// in Deps.CollectionLedgerBackend. app_settings has no RLS, so a plain pool query is correct.
func LoadCollectionLedgerBackend(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var v string
	err := pool.QueryRow(ctx, `SELECT value FROM app_settings WHERE key = $1`, collectionLedgerMarkerKey).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// leadgenRoutesEnabled reports whether the leadgen routes may register: GO_COLLECTION_ROUTES_ENABLED
// and PG_LEAD_STORE on and the ledger marker "postgres". Otherwise nothing registers (requests fall
// through to Python) and the reason is logged, so a canary cannot be enabled by mistake.
func (d Deps) leadgenRoutesEnabled(area string) bool {
	if !d.Flags.Leadgen.GoCollectionRoutesEnabled {
		d.logger().Warn("leadgen routes not registered: GO_COLLECTION_ROUTES_ENABLED is false", "area", area)
		return false
	}
	if !d.Flags.PGLeadStore {
		d.logger().Warn("leadgen routes not registered: PG_LEAD_STORE is false (Go supports the Postgres lead store only)", "area", area)
		return false
	}
	if d.CollectionLedgerBackend != "postgres" {
		d.logger().Warn("leadgen routes not registered: collection ledger marker is not postgres (run the ledger cutover)", "area", area)
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
