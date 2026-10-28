//go:build integration

package providerrun_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// A manifest provider's person-level field is cached per person: a repeat for the same person is
// served from the cache (no vendor call), a second person at the same company is a fresh lookup.
func TestManifestMobileCachedPerPerson(t *testing.T) {
	sim := newVendorSim(t, func(string, map[string]any) string { return `{"message":"ok","mobile_number":"+15550001"}` })
	ws := uniq("t18wsM")
	h := newHarness(t, func(d *providerrun.Deps) {
		d.HTTP = sim.client()
		d.Manifests = builtinIndex(t)
		d.URLCheck = func(string) error { return nil }
	})
	h.sec.vals[ws+"|LEADMAGIC_API_KEY"] = "lm-key"
	t.Cleanup(func() {
		if owner, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL")); err == nil {
			_, _ = owner.Exec(context.Background(), `DELETE FROM enrichment_cache WHERE workspace_id = $1`, ws)
			owner.Close()
		}
	})
	lead := func(person string) providers.Input {
		return providers.Input{Field: "mobile_phone", Lead: map[string]any{
			"website": "acme.com", "company": "Acme", "contact_person": person,
			"linkedin_url": "https://www.linkedin.com/in/" + person,
		}}
	}
	run := func(person string) {
		t.Helper()
		out, err := h.r.Run(context.Background(), ws, "leadmagic_mobile", lead(person), 0)
		if err != nil || out.Error != "" || out.Value != "+15550001" {
			t.Fatalf("%s: %+v %v", person, out, err)
		}
	}
	run("ada")
	run("ada")
	if n := len(sim.calls()); n != 1 {
		t.Fatalf("same person twice: %d vendor calls, want 1 (cache)", n)
	}
	run("grace")
	if n := len(sim.calls()); n != 2 {
		t.Fatalf("second person at the company must be its own lookup: %d vendor calls, want 2", n)
	}
}
