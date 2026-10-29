package vendorcatalog

import "testing"

func TestNativeFreeProvidersHaveExplicitZeroCost(t *testing.T) {
	SetDeclaredCost(nil)
	t.Cleanup(func() { SetDeclaredCost(nil) })
	for _, name := range []string{"website_scraper", "lead_scorer", "companies_house"} {
		if !HasKnownCost(name) || BaseCost(name) != 0 || IsPaid(name) {
			t.Errorf("native provider %s should have a known zero platform cost", name)
		}
	}
	if HasKnownCost("qa_unknown_provider") {
		t.Fatal("unknown providers must remain unknown")
	}
}

func TestNativeFreeMetadataDoesNotOverrideDeclaredPaidCost(t *testing.T) {
	SetDeclaredCost(func(name string) (float64, bool) { return 0.25, name == "website_scraper" })
	t.Cleanup(func() { SetDeclaredCost(nil) })
	if !HasKnownCost("website_scraper") || BaseCost("website_scraper") != 0.25 || !IsPaid("website_scraper") {
		t.Fatal("declared provider pricing must take precedence over native-free metadata")
	}
}
