package api

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencerefresh"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destdeliver"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// audiencePatterns / destinationPatterns are the Python route sets. Stubs register nothing until their task lands, so the registered
// set must stay a subset.
// Generated from legacy/python/apps/api/routers/{audiences,destinations}.py (9 and 16 routes).
var audiencePatterns = map[string]bool{
	"GET /api/audiences":                        true,
	"GET /api/audiences/capabilities":           true,
	"POST /api/audiences":                       true,
	"PATCH /api/audiences/{audience_id}":        true,
	"POST /api/audiences/{audience_id}/refresh": true,
	"GET /api/audiences/{audience_id}/members":  true,
	"GET /api/audiences/{audience_id}/accounts": true,
	"GET /api/audiences/{audience_id}/events":   true,
	"DELETE /api/audiences/{audience_id}":       true,
}

var destinationPatterns = map[string]bool{
	"GET /api/audience-destinations/access":                            true, // Workbench extension; effective permission, not a Python parity route.
	"GET /api/audience-destinations/types":                             true,
	"GET /api/audience-destinations":                                   true,
	"POST /api/audience-destinations":                                  true,
	"PATCH /api/audience-destinations/{destination_id}":                true,
	"POST /api/audience-destinations/{destination_id}/sync":            true,
	"GET /api/audience-destinations/{destination_id}/runs":             true,
	"GET /api/audience-destinations/{destination_id}/health":           true,
	"GET /api/audience-destinations/runs/{run_id}/deliveries":          true,
	"POST /api/audience-destinations/runs/{run_id}/retry":              true,
	"POST /api/audience-destinations/runs/{run_id}/cancel":             true,
	"POST /api/audience-destinations/{destination_id}/inbound-token":   true,
	"GET /api/audience-destinations/{destination_id}/inbound-token":    true,
	"DELETE /api/audience-destinations/{destination_id}/inbound-token": true,
	"GET /api/audience-destinations/{destination_id}/inbound-receipts": true,
	"POST /api/audience-destinations/inbound/{destination_id}":         true,
	"DELETE /api/audience-destinations/{destination_id}":               true,
}

type nopEmitter struct{}

func (nopEmitter) AudienceEvents(context.Context, pgx.Tx, string, []automationhook.AudienceEvent) (int, error) {
	return 0, nil
}

// The plan 08 emitter satisfies the seam directly, no adapter.
var _ audiencerefresh.Emitter = automationhook.Emitter(nil)

func audDeps(pg, on bool, em audiencerefresh.Emitter) Deps {
	return Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), AudienceEvents: em,
		Flags: config.Flags{PGLeadStore: pg, Audiences: config.AudienceSettings{GoAudienceRoutesEnabled: on, GoDestRoutesEnabled: on}}}
}

func TestAudienceRoutesNotRegisteredWithoutFlagOrEmitter(t *testing.T) {
	for _, c := range []struct {
		pg, on bool
		em     audiencerefresh.Emitter
	}{{false, false, nopEmitter{}}, {true, false, nopEmitter{}}, {false, true, nopEmitter{}}, {true, true, nil}} {
		r := httpx.NewRouter()
		RegisterAudiences(r, audDeps(c.pg, c.on, c.em))
		if n := len(r.Patterns()); n != 0 {
			t.Errorf("pg=%v on=%v emitter=%v: %d routes registered: %v", c.pg, c.on, c.em != nil, n, r.Patterns())
		}
	}
	RegisterAll(httpx.NewRouter(), audDeps(true, false, nil)) // must not panic
}

func TestDestinationRoutesNotRegisteredWithoutFlag(t *testing.T) {
	for _, c := range []struct{ pg, on bool }{{false, false}, {true, false}, {false, true}} {
		r := httpx.NewRouter()
		RegisterDestinations(r, audDeps(c.pg, c.on, nil))
		if n := len(r.Patterns()); n != 0 {
			t.Errorf("pg=%v on=%v: %d routes registered: %v", c.pg, c.on, n, r.Patterns())
		}
	}
}

func TestAudienceRoutePatterns(t *testing.T) {
	if len(audiencePatterns) != 9 {
		t.Fatalf("pattern table has %d entries, want 9", len(audiencePatterns))
	}
	r := httpx.NewRouter()
	RegisterAudiences(r, audDeps(true, true, nopEmitter{}))
	for _, p := range r.Patterns() {
		if !audiencePatterns[p] {
			t.Errorf("unexpected audience pattern %s", p)
		}
	}
}

func TestDestinationRoutePatterns(t *testing.T) {
	if len(destinationPatterns) != 17 {
		t.Fatalf("pattern table has %d entries, want 17", len(destinationPatterns))
	}
	r := httpx.NewRouter()
	RegisterDestinations(r, audDeps(true, true, nil))
	for _, p := range r.Patterns() {
		if !destinationPatterns[p] {
			t.Errorf("unexpected destination pattern %s", p)
		}
	}
	_ = destdeliver.Ports{}
}
