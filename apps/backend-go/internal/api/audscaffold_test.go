package api

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencerefresh"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destdeliver"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// audiencePatterns are exactly the 8 routes of plan 10 Tasks 15-17; destinationPatterns the
// 15 routes of Tasks 18-21. Stubs register nothing until their task lands, so the registered
// set must stay a subset.
var audiencePatterns = map[string]bool{
	"GET /api/audiences":                        true,
	"POST /api/audiences":                       true,
	"GET /api/audiences/{audience_id}":          true,
	"PUT /api/audiences/{audience_id}":          true,
	"DELETE /api/audiences/{audience_id}":       true,
	"GET /api/audiences/{audience_id}/members":  true,
	"GET /api/audiences/{audience_id}/events":   true,
	"GET /api/audiences/{audience_id}/accounts": true,
}

var destinationPatterns = map[string]bool{
	"GET /api/audience-destinations":                                               true,
	"POST /api/audience-destinations":                                              true,
	"GET /api/audience-destinations/{destination_id}":                              true,
	"PUT /api/audience-destinations/{destination_id}":                              true,
	"DELETE /api/audience-destinations/{destination_id}":                           true,
	"POST /api/audience-destinations/{destination_id}/sync":                        true,
	"GET /api/audience-destinations/{destination_id}/runs":                         true,
	"GET /api/audience-destinations/{destination_id}/runs/{run_id}":                true,
	"GET /api/audience-destinations/{destination_id}/runs/{run_id}/deliveries":     true,
	"GET /api/audience-destinations/{destination_id}/inbound-tokens":               true,
	"POST /api/audience-destinations/{destination_id}/inbound-tokens":              true,
	"DELETE /api/audience-destinations/{destination_id}/inbound-tokens/{token_id}": true,
	"POST /api/audience-destinations/inbound/{destination_id}":                     true,
	"GET /api/audience-destinations/{destination_id}/inbound-receipts":             true,
	"POST /api/audience-destinations/{destination_id}/test":                        true,
}

type nopEmitter struct{}

func (nopEmitter) Emit(context.Context, pgx.Tx, string, []audiencerefresh.MembershipEvent) error {
	return nil
}

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
	if len(audiencePatterns) != 8 {
		t.Fatalf("pattern table has %d entries, want 8", len(audiencePatterns))
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
	if len(destinationPatterns) != 15 {
		t.Fatalf("pattern table has %d entries, want 15", len(destinationPatterns))
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
