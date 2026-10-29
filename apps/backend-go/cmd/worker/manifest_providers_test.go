package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/runplan"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/vendorcatalog"
)

type emptySecrets struct{}

func (emptySecrets) Get(context.Context, string, string) (string, string, error) { return "", "", nil }

// TestWorkerProviderRunnerServesManifestProviders (final review C2): the worker's provider runner
// executes the built-in declarative providers (no Go implementation), so the default mobile_phone
// waterfall and every providerrun caller can run them; an unknown name stays unavailable. The
// worker also wires the vendor catalog to the connector index, so a refresh it plans prices a
// connector by its declared cost_per_lookup like the API does.
func TestWorkerProviderRunnerServesManifestProviders(t *testing.T) {
	runplan.ResetWiredForTest()
	t.Cleanup(runplan.ResetWiredForTest)
	r := workerProviderRunner(config.Config{AppEnv: "test"}, nil, emptySecrets{}, nil, slog.Default())
	for _, name := range []string{"leadmagic_mobile", "prospeo_mobile", "leadmagic_email", "leadmagic_company", "hunter_io"} {
		if !r.Serves(name) {
			t.Errorf("worker runner does not serve %s", name)
		}
	}
	if r.Serves("no_such_vendor") {
		t.Error("unknown provider served")
	}
	if !runplan.Wired() {
		t.Fatal("the worker did not wire the vendor catalog to the connector index")
	}
	if !vendorcatalog.HasKnownCost("leadmagic_company") {
		t.Error("declared manifest cost unknown in the worker")
	}
}
