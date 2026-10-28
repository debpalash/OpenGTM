package api

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/jackc/pgx/v5/pgxpool"
)

// enrichPatterns are exactly the routes Tasks 23-30 plus GET /api/signals/sources
// (Task 17) register. Stubs register nothing until their task lands, so the
// registered set must always be a subset of this table.
var enrichPatterns = map[string]bool{
	"GET /api/workbooks/{workbook_id}/run/estimate":                      true,
	"POST /api/workbooks/{workbook_id}/stop":                             true,
	"GET /api/workbooks/meta/provider-stats":                             true,
	"POST /api/workbooks/{workbook_id}/run":                              true,
	"POST /api/workbooks/{workbook_id}/rows/{row_id}/cells/{col_id}/run": true,
	"POST /api/workbooks/{workbook_id}/sources":                          true,
	"POST /api/workbooks/{workbook_id}/sources/{col_id}/run":             true,
	"PUT /api/workbooks/{workbook_id}/refresh-policy":                    true,
	"POST /api/workbooks/{workbook_id}/refresh":                          true,
	"POST /api/workbooks/{workbook_id}/migrate":                          true,
	"GET /api/workbooks/{workbook_id}/ws":                                true,
	"POST /api/v2/workbooks/{workbook_id}/ingest-token":                  true,
	"POST /api/v2/workbooks/{workbook_id}/rows/ingest":                   true,
	"GET /api/signals/sources":                                           true,
}

// retainedEnrichRoutes stay on FastAPI and must never register here.
// The source preview moved to Go in plan 15 (GO_SOURCE_PREVIEW_ENABLED); none remain.
var retainedEnrichRoutes = []string{}

func enrichDeps(pg, run bool) Deps {
	return Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Flags: config.Flags{PGLeadStore: pg, GoRunRoutesEnabled: run}}
}

func registerEnrich(r *httpx.Router, d Deps) {
	RegisterWorkbookRuns(r, d)
	RegisterIngest(r, d)
	RegisterWorkbookWS(r, d)
}

func TestEnrichRoutesNotRegisteredWithoutFlags(t *testing.T) {
	for _, c := range []struct{ pg, run bool }{{false, false}, {true, false}, {false, true}} {
		r := httpx.NewRouter()
		registerEnrich(r, enrichDeps(c.pg, c.run))
		if n := len(r.Patterns()); n != 0 {
			t.Errorf("pg=%v run=%v: %d routes registered: %v", c.pg, c.run, n, r.Patterns())
		}
	}
	if enrichDeps(true, false).runRoutesEnabled("x") || enrichDeps(false, true).runRoutesEnabled("x") {
		t.Fatal("runRoutesEnabled needs both flags")
	}
	if !enrichDeps(true, true).runRoutesEnabled("x") {
		t.Fatal("runRoutesEnabled false with both flags")
	}
}

// enrichDepsWithPool carries the minimum every register function needs (a lazy pool and an identity
// store); without them each one returns before registering anything.
func enrichDepsWithPool(t *testing.T) Deps {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	d := enrichDeps(true, true)
	d.Pool, d.Identity = pool, identity.NewStore(pool)
	d.Flags.IngestAPIEnabled = true
	return d
}

// TestEnrichRoutePatterns: the registered set EQUALS the table (a missing route fails as loudly as an
// unexpected one) and no retained route is registered.
func TestEnrichRoutePatterns(t *testing.T) {
	if len(enrichPatterns) != 14 {
		t.Fatalf("pattern table has %d entries, want 14", len(enrichPatterns))
	}
	r := httpx.NewRouter()
	registerEnrich(r, enrichDepsWithPool(t))
	got := map[string]bool{}
	for _, p := range r.Patterns() {
		got[p] = true
		if p == "GET /api/workbooks/{workbook_id}/sources/{col_id}/preview" {
			continue // the source preview (GO_SOURCE_PREVIEW_ENABLED, retired: always on) is covered by TestEveryEnrichmentRouteIsPortedOrRetained
		}
		if !enrichPatterns[p] {
			t.Errorf("unexpected pattern %s", p)
		}
		for _, ret := range retainedEnrichRoutes {
			if p == ret {
				t.Errorf("retained route registered: %s", p)
			}
		}
	}
	for p := range enrichPatterns {
		if !got[p] {
			t.Errorf("pattern %s is in the table but not registered", p)
		}
	}
}

func TestEnrichRegisteredViaRegisterAll(t *testing.T) {
	r := httpx.NewRouter()
	RegisterAll(r, enrichDeps(false, true))
	for _, p := range r.Patterns() {
		if enrichPatterns[p] {
			t.Errorf("enrichment route registered without PG_LEAD_STORE: %s", p)
		}
	}
}
