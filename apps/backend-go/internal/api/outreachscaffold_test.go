package api

import (
	"io"
	"log/slog"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// outreachPatterns are exactly the 22 routes of plan 09 Tasks 20-25. Stubs register
// nothing until their task lands, so the registered set must stay a subset.
var outreachPatterns = map[string]bool{
	"GET /api/outreach/sequences":                   true,
	"POST /api/outreach/sequences":                  true,
	"GET /api/outreach/sequences/{seq_id}":          true,
	"PUT /api/outreach/sequences/{seq_id}":          true,
	"DELETE /api/outreach/sequences/{seq_id}":       true,
	"POST /api/outreach/sequences/{seq_id}/start":   true,
	"POST /api/outreach/sequences/{seq_id}/pause":   true,
	"POST /api/outreach/sequences/{seq_id}/enroll":  true,
	"POST /api/outreach/sequences/{seq_id}/execute": true,
	"GET /api/outreach/sequences/{seq_id}/stats":    true,
	"GET /api/outreach/sequences/{seq_id}/sends":    true,
	"GET /api/outreach/smtp/status":                 true,
	"PUT /api/outreach/smtp/config":                 true,
	"POST /api/outreach/smtp/test":                  true,
	"GET /api/outreach/suppressions":                true,
	"POST /api/outreach/suppressions":               true,
	"DELETE /api/outreach/suppressions/{email}":     true,
	"GET /api/outreach/drafts":                      true,
	"GET /api/outreach/drafts/{draft_id}":           true,
	"GET /api/outreach/unsubscribe":                 true,
	"POST /api/outreach/unsubscribe":                true,
	"POST /api/outreach/webhooks/bounce":            true,
}

func outreachDeps(pg, on bool) Deps {
	return Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Flags: config.Flags{PGLeadStore: pg, Outreach: config.OutreachSettings{GoRoutesEnabled: on}}}
}

func TestOutreachRoutesNotRegisteredWithoutFlags(t *testing.T) {
	for _, c := range []struct{ pg, on bool }{{false, false}, {true, false}, {false, true}} {
		r := httpx.NewRouter()
		RegisterOutreach(r, outreachDeps(c.pg, c.on))
		if n := len(r.Patterns()); n != 0 {
			t.Errorf("pg=%v on=%v: %d routes registered: %v", c.pg, c.on, n, r.Patterns())
		}
	}
	if outreachDeps(true, false).outreachRoutesEnabled("x") || outreachDeps(false, true).outreachRoutesEnabled("x") ||
		!outreachDeps(true, true).outreachRoutesEnabled("x") {
		t.Fatal("outreachRoutesEnabled needs PG_LEAD_STORE and GO_OUTREACH_ROUTES_ENABLED")
	}
	RegisterAll(httpx.NewRouter(), outreachDeps(true, false)) // must not panic
}

func TestOutreachRoutePatterns(t *testing.T) {
	if len(outreachPatterns) != 22 {
		t.Fatalf("pattern table has %d entries, want 22", len(outreachPatterns))
	}
	r := httpx.NewRouter()
	RegisterOutreach(r, outreachDeps(true, true))
	for _, p := range r.Patterns() {
		if !outreachPatterns[p] {
			t.Errorf("unexpected outreach pattern %s", p)
		}
	}
}
