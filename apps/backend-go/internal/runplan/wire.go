package runplan

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/billing"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/vendorcatalog"
)

// WireDeps are the sources the vendor catalog and billing projection read.
type WireDeps struct {
	// Index returns the current manifest index (manifest.LoadIndex); declared
	// cost_per_lookup values come from its entries.
	Index func() map[string]manifest.Entry
	// AppSetting reads a global setting with the environment fallback
	// ((*settings.App).Get); it backs the RESEARCH_VENDOR_COST flag.
	AppSetting func(ctx context.Context, key, def string) (string, error)
}

var wired atomic.Bool

// Wired reports whether Wire ran in this process (startup tests assert it).
func Wired() bool { return wired.Load() }

// ResetWiredForTest clears the Wired flag (tests only).
func ResetWiredForTest() { wired.Store(false) }

// researchSettingTimeout bounds the settings read behind each research cost lookup.
const researchSettingTimeout = 2 * time.Second

// Wire connects the vendor catalog to the manifest index and the settings
// store, and billing's platform-cost projection to the vendor catalog (without
// it only `research` is platform billed). Call it once at startup, before
// serving; it replaces the package-level hooks of vendorcatalog and billing.
func Wire(d WireDeps) {
	wired.Store(true)
	vendorcatalog.SetDeclaredCost(func(name string) (float64, bool) {
		if d.Index == nil {
			return 0, false
		}
		e, ok := d.Index()[name]
		if !ok {
			return 0, false
		}
		return e.Manifest.CostPerLookup, true
	})
	vendorcatalog.SetResearchFlag(func() bool { return researchEnabled(d.AppSetting) })
	billing.SetCostCatalog(catalog{})
}

// researchEnabled is vendor_catalog._research_vendor_cost_enabled: the global
// setting (which itself falls back to the environment), and on any read error
// the environment alone. Anything but 0/false/off/no/"" is on.
func researchEnabled(read func(context.Context, string, string) (string, error)) bool {
	val, err := "", error(nil)
	if read != nil {
		ctx, cancel := context.WithTimeout(context.Background(), researchSettingTimeout)
		defer cancel()
		val, err = read(ctx, "RESEARCH_VENDOR_COST", "0")
	}
	if read == nil || err != nil {
		val = "0"
		if e, ok := os.LookupEnv("RESEARCH_VENDOR_COST"); ok {
			val = e
		}
	}
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "0", "false", "off", "no", "":
		return false
	}
	return true
}

// catalog adapts the vendor catalog to billing.CostCatalog: a provider is
// platform billed when it is paid and not BYOK (service.projected_platform_cost).
type catalog struct{}

func (catalog) PlatformCost(provider string) (float64, bool) {
	if vendorcatalog.IsPaid(provider) && !vendorcatalog.IsBYOK(provider) {
		return vendorcatalog.BaseCost(provider), true
	}
	return 0, false
}
