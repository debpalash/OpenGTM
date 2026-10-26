package api

import (
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// Derived from legacy/python/apps/api/routers/leads.py (jobs_router and the /leads/* utilities,
// prefix /api) and routers/settings.py (prefix /api/settings). The brief's count (19) matches.
// NOT registered (retained on FastAPI): POST /api/lead/{id}/enrich, GET /api/v2/search/unified,
// POST /api/v2/scraper/scrape, /sources/{col}/preview, and the internal POST /internal/leadgen/system-stats.
var leadgenCollectPatterns = []string{"POST /api/collect"}

var leadgenJobsPatterns = []string{
	"DELETE /api/jobs/{job_id}",
	"GET /api/jobs",
	"GET /api/jobs/{job_id}",
	"GET /api/jobs/{job_id}/events",
	"GET /api/jobs/{job_id}/leads",
	"GET /api/jobs/{job_id}/stages",
	"GET /api/system-stats",
	"POST /api/jobs/{job_id}/cancel",
	"POST /api/jobs/{job_id}/retry",
}

var leadgenUtilPatterns = []string{
	"POST /api/leads/domain-intel",
	"POST /api/leads/score",
	"POST /api/leads/tech-stack",
	"POST /api/leads/verify-email",
}

var leadgenSourcePatterns = []string{
	"GET /api/settings/sources",
	"GET /api/settings/sources/health",
	"GET /api/settings/sources/registry",
	"POST /api/settings/sources/{source_id}/health/reset",
	"PUT /api/settings/sources/{source_id}",
}

func leadgenDeps(pg, on bool, backend string) Deps {
	return Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), CollectionLedgerBackend: backend,
		Flags: config.Flags{PGLeadStore: pg, Leadgen: config.LeadgenSettings{GoCollectionRoutesEnabled: on}}}
}

func registerLeadgen(r *httpx.Router, d Deps) {
	RegisterCollect(r, d)
	RegisterJobs(r, d)
	RegisterLeadUtils(r, d)
	RegisterSourceSettings(r, d)
}

func TestLeadgenRoutesNotRegisteredWithoutFlags(t *testing.T) {
	for name, d := range map[string]Deps{
		"flag off":      leadgenDeps(true, false, "postgres"),
		"pg store off":  leadgenDeps(false, true, "postgres"),
		"both off":      leadgenDeps(false, false, "postgres"),
		"marker absent": leadgenDeps(true, true, ""),
	} {
		r := httpx.NewRouter()
		registerLeadgen(r, d)
		if n := len(r.Patterns()); n != 0 {
			t.Errorf("%s: %d routes registered: %v", name, n, r.Patterns())
		}
		RegisterAll(httpx.NewRouter(), d) // must not panic
	}
}

func TestLedgerMarkerRequired(t *testing.T) {
	for _, m := range []string{"", "sqlite", "Postgres", "postgres "} {
		r := httpx.NewRouter()
		registerLeadgen(r, leadgenDeps(true, true, m))
		if n := len(r.Patterns()); n != 0 {
			t.Errorf("marker %q registered %d routes", m, n)
		}
	}
	r := httpx.NewRouter()
	registerLeadgen(r, leadgenDeps(true, true, "postgres"))
	if len(r.Patterns()) == 0 {
		t.Error("marker postgres must register")
	}
}

func TestLeadgenRoutePatterns(t *testing.T) {
	d := leadgenDeps(true, true, "postgres")
	for name, c := range map[string]struct {
		reg  func(*httpx.Router, Deps)
		want []string
	}{
		"collect": {RegisterCollect, leadgenCollectPatterns},
		"jobs":    {RegisterJobs, leadgenJobsPatterns},
		"utils":   {RegisterLeadUtils, leadgenUtilPatterns},
		"sources": {RegisterSourceSettings, leadgenSourcePatterns},
	} {
		r := httpx.NewRouter()
		c.reg(r, d)
		if got := r.Patterns(); !reflect.DeepEqual(got, sorted(c.want)) {
			t.Errorf("%s patterns\n got %v\nwant %v", name, got, sorted(c.want))
		}
	}
	if n := len(leadgenCollectPatterns) + len(leadgenJobsPatterns) + len(leadgenUtilPatterns) + len(leadgenSourcePatterns); n != 19 {
		t.Errorf("plan 14 ports 19 routes, table has %d", n)
	}
	// Whole group at once: no duplicate pattern panic against the other registered groups.
	registerLeadgen(httpx.NewRouter(), d)
}
