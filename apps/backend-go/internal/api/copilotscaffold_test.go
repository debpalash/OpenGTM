package api

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
)

// Derived from legacy/python/apps/api/routers/copilotkit.py (prefix /api/copilotkit; approvals ride
// on POST /api/copilotkit, there is no separate approval route) and person_intel.py, and checked
// against the recorded fixtures under testdata/contract/{copilot,person}.
var copilotPatterns = []string{
	"DELETE /api/copilotkit/conversations/{conv_id}",
	"GET /api/copilotkit/conversations",
	"GET /api/copilotkit/conversations/{conv_id}",
	"GET /api/copilotkit/info",
	"GET /api/copilotkit/memories",
	"POST /api/copilotkit",
	"POST /api/copilotkit/info",
}

var personIntelPatterns = []string{
	"DELETE /api/person/{record_id}",
	"GET /api/person/history",
	"GET /api/person/{record_id}",
	"GET /ws/person/enrich",
	"POST /api/person/enrich",
}

// copilotDeps builds route deps; engine adds what the in-process chat engine needs (settings registry, secrets
// envelope and a pool; the pool never connects in these tests).
func copilotDeps(pg, on bool, engine bool) Deps {
	d := Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Flags: config.Flags{PGLeadStore: pg, Copilot: config.CopilotSettings{GoCopilotEnabled: on}}}
	if engine {
		d = withChatEngineDeps(d)
	}
	return d
}

func withChatEngineDeps(d Deps) Deps {
	reg, err := settings.Load()
	if err != nil {
		panic(err)
	}
	env, err := secrets.NewEnvelope(secrets.Config{MasterKey: "k", AppEnv: "dev"}, nil)
	if err != nil {
		panic(err)
	}
	if d.Pool == nil {
		pool, err := pgxpool.New(context.Background(), "postgres://coverage@127.0.0.1:1/coverage")
		if err != nil {
			panic(err)
		}
		d.Pool = pool
	}
	d.Registry, d.Envelope = reg, env
	return d
}

func sorted(s []string) []string { c := append([]string(nil), s...); sort.Strings(c); return c }

func TestCopilotRoutesNotRegisteredWithoutFlag(t *testing.T) {
	r := httpx.NewRouter()
	RegisterCopilot(r, copilotDeps(true, false, true))
	RegisterPersonIntel(r, copilotDeps(true, false, true))
	if n := len(r.Patterns()); n != 0 {
		t.Fatalf("%d routes registered without COPILOT_GO_ENABLED: %v", n, r.Patterns())
	}
	RegisterAll(httpx.NewRouter(), copilotDeps(true, false, true)) // must not panic
}

func TestCopilotRoutePatterns(t *testing.T) {
	r := httpx.NewRouter()
	RegisterCopilot(r, copilotDeps(true, true, true))
	if got := r.Patterns(); !reflect.DeepEqual(got, sorted(copilotPatterns)) {
		t.Errorf("copilot patterns\n got %v\nwant %v", got, sorted(copilotPatterns))
	}
	r = httpx.NewRouter()
	RegisterPersonIntel(r, copilotDeps(true, true, true))
	if got := r.Patterns(); !reflect.DeepEqual(got, sorted(personIntelPatterns)) {
		t.Errorf("person patterns\n got %v\nwant %v", got, sorted(personIntelPatterns))
	}
	if len(copilotPatterns)+len(personIntelPatterns) != 12 {
		t.Error("plan 12 ports 7+5 routes")
	}
}

// Chat (POST /api/copilotkit) needs the in-process engine's settings; the REST history routes and person intel
// do not. No Python worker is involved (plan 15).
func TestCopilotChatRequiresEngineSettings(t *testing.T) {
	r := httpx.NewRouter()
	RegisterCopilot(r, copilotDeps(true, true, false))
	want := []string{}
	for _, p := range sorted(copilotPatterns) {
		if p != "POST /api/copilotkit" {
			want = append(want, p)
		}
	}
	if got := r.Patterns(); !reflect.DeepEqual(got, want) {
		t.Errorf("without the engine settings\n got %v\nwant %v", got, want)
	}
}

func TestCopilotRequiresPGLeadStore(t *testing.T) {
	r := httpx.NewRouter()
	RegisterCopilot(r, copilotDeps(false, true, true))
	RegisterPersonIntel(r, copilotDeps(false, true, true))
	if n := len(r.Patterns()); n != 0 {
		t.Fatalf("%d routes registered with PG_LEAD_STORE=false", n)
	}
}
