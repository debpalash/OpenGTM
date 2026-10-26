//go:build integration

package api

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func newSourcesEnv(t *testing.T) sigEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Flags: config.Flags{PGLeadStore: true}}
	registerSignalSources(r, d)
	e := sigEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}
	e.purge()
	t.Cleanup(e.purge)
	e.seedSignals(t) // users and workspaces only matter here
	return e
}

// TestSignalSourcesMatchesFixture compares GET /api/signals/sources with the FastAPI recordings
// (testdata/contract/signal_sources/sources_*.json). No certification evidence is configured, so every
// source is "beta" with certification null, as in the recording.
func TestSignalSourcesMatchesFixture(t *testing.T) {
	e := newSourcesEnv(t)
	for _, c := range []struct{ name, who string }{{"signal_sources/sources_catalog", "alice"}, {"signal_sources/sources_no_auth", ""}} {
		fx := contract.Load(t, c.name)
		_, got := e.req("GET", "/api/signals/sources", c.who, "", "")
		contract.AssertEqual(t, got, fx.Response)
	}
}
