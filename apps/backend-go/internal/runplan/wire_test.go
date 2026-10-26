package runplan

import (
	"context"
	"errors"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/billing"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/vendorcatalog"
)

type noCatalog struct{}

func (noCatalog) PlatformCost(string) (float64, bool) { return 0, false }

func resetWiringForTest() {
	vendorcatalog.SetDeclaredCost(nil)
	vendorcatalog.SetResearchFlag(nil)
	billing.SetCostCatalog(noCatalog{})
}

func resetWiring(t *testing.T) {
	t.Helper()
	t.Cleanup(resetWiringForTest)
}

// TestWireDeclaredCostAndCatalog: declared manifest costs reach the vendor
// catalog and the billing projection; research follows the global setting;
// BYOK catalog vendors stay unbilled.
func TestWireDeclaredCostAndCatalog(t *testing.T) {
	resetWiring(t)
	idx := map[string]manifest.Entry{
		"my_declared": {Manifest: manifest.Manifest{Name: "my_declared", CostPerLookup: 0.25}, SHA256: "x"},
		"my_free":     {Manifest: manifest.Manifest{Name: "my_free"}, SHA256: "y"},
	}
	setting := "0"
	Wire(WireDeps{
		Index: func() map[string]manifest.Entry { return idx },
		AppSetting: func(_ context.Context, key, def string) (string, error) {
			if key != "RESEARCH_VENDOR_COST" {
				t.Errorf("unexpected key %q", key)
			}
			return setting, nil
		},
	})
	if c := vendorcatalog.BaseCost("my_declared"); c != 0.25 {
		t.Fatalf("declared cost ignored: %v", c)
	}
	if vendorcatalog.BaseCost("my_free") != 0 || !vendorcatalog.HasKnownCost("my_free") {
		t.Fatal("a zero declared cost is known and free")
	}
	// declared providers are not in the catalog table: BYOK by default, so never debited.
	if got := billing.ProjectedPlatformCost(10, map[string][]string{"c": {"my_declared", "hunter_io"}}, nil); got != 0 {
		t.Fatalf("byok must not bill, got %v", got)
	}
	if got := billing.ProjectedPlatformCost(10, map[string][]string{"c": {"research"}}, nil); got != 0 {
		t.Fatalf("research off must not bill, got %v", got)
	}
	setting = "true"
	if got := billing.ProjectedPlatformCost(10, map[string][]string{"c": {"research", "claygent"}}, nil); got != 0.5 {
		t.Fatalf("research on: want 0.5 (claygent is not a catalog vendor, so BYOK) got %v", got)
	}
}

func TestWireResearchFlagFallsBackToEnv(t *testing.T) {
	resetWiring(t)
	t.Setenv("RESEARCH_VENDOR_COST", "yes")
	Wire(WireDeps{
		Index:      func() map[string]manifest.Entry { return nil },
		AppSetting: func(context.Context, string, string) (string, error) { return "", errors.New("db down") },
	})
	if vendorcatalog.BaseCost("research") != 0.05 {
		t.Fatal("a failing settings read must fall back to the environment")
	}
	t.Setenv("RESEARCH_VENDOR_COST", "off")
	if vendorcatalog.BaseCost("research") != 0 {
		t.Fatal("env off")
	}
}
