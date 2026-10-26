package api

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// leadsSignalsPatterns are the 15 lead + 5 signal + 1 event routes plan 05 ports.
var leadsSignalsPatterns = map[string]bool{
	"GET /api/leads": true, "GET /api/stats": true, "GET /api/filters": true,
	"GET /api/lead/{lead_id}": true, "GET /api/lead/{lead_id}/timeline": true,
	"GET /api/lead/{lead_id}/similar": true, "POST /api/lead/{lead_id}/status": true,
	"PUT /api/lead/{lead_id}": true, "DELETE /api/lead/{lead_id}": true, "POST /api/lead": true,
	"GET /api/export/csv": true, "POST /api/leads/dedup": true, "POST /api/leads/dedup/merge": true,
	"POST /api/leads/bulk-enrich": true,
	"GET /api/signals":            true, "GET /api/signals/analytics": true, "GET /api/signals/counts": true,
	"POST /api/signals/scan": true, "POST /api/signals/mark-read": true,
	"GET /api/events": true,
}

// retained stay on FastAPI (constraints.md); they must never be registered here.
var retainedLeadsRoutes = []string{
	"POST /api/lead/{lead_id}/enrich", "POST /api/collect", "GET /api/jobs", "GET /api/jobs/{job_id}",
	"GET /api/system-stats", "POST /api/v2/scraper/scrape", "GET /api/v2/search/unified",
	"POST /api/leads/verify-email", "POST /api/leads/score", "POST /api/leads/tech-stack",
	"POST /api/leads/domain-intel", "GET /api/signals/sources",
}

func TestLeadsNotRegisteredWithoutPGStore(t *testing.T) {
	d := Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Flags: config.Flags{PGLeadStore: false}}
	r := httpx.NewRouter()
	RegisterLeads(r, d)
	RegisterSignals(r, d)
	RegisterEvents(r, d)
	for _, p := range r.Patterns() {
		if leadsSignalsPatterns[p] {
			t.Errorf("registered without PG_LEAD_STORE: %s", p)
		}
	}
}

func TestLeadsRegisteredPatterns(t *testing.T) {
	d := Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Flags: config.Flags{PGLeadStore: true}}
	r := httpx.NewRouter()
	RegisterLeads(r, d)
	RegisterSignals(r, d)
	RegisterEvents(r, d)
	for _, p := range r.Patterns() {
		if !leadsSignalsPatterns[p] {
			t.Errorf("unexpected pattern %s", p)
		}
		for _, ret := range retainedLeadsRoutes {
			if p == ret {
				t.Errorf("retained route registered: %s", p)
			}
		}
	}
	if len(leadsSignalsPatterns) != 20 {
		t.Fatalf("pattern table has %d entries, want 20", len(leadsSignalsPatterns))
	}
	_ = strings.TrimSpace
}
