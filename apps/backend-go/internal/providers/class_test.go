package providers_test

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	_ "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/contact"
	_ "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/firmo"
	_ "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/scrape"
)

// Every registered provider declares what a timeout from it means; a new provider without a class fails here.
func TestEveryRegisteredProviderHasACooldownClass(t *testing.T) {
	for _, name := range providers.Names() {
		if providers.ClassOfName(name) == providers.ClassUnclassified {
			t.Errorf("provider %q has no cooldown class: embed providers.Upstream, providers.TargetSite or providers.Local", name)
		}
	}
}

// Providers that fetch the lead's own website, or its mail server, time out because of that one lead.
func TestTargetDependentProvidersAreClassifiedAsTargetSite(t *testing.T) {
	for _, name := range []string{"website_scraper", "deep_scraper", "jsonld_firmographics", "tech_stack", "mailscout"} {
		if got := providers.ClassOfName(name); got != providers.ClassTargetSite {
			t.Errorf("%s: class %v, want target site", name, got)
		}
	}
}

// Providers that call one vendor API or search backend for every lead share that upstream's health.
func TestSingleUpstreamProvidersAreClassifiedAsUpstream(t *testing.T) {
	for _, name := range []string{"wikidata", "ddg_company", "ddg_email", "hunter_io", "ats_hiring", "social_finder", "local_business"} {
		if got := providers.ClassOfName(name); got != providers.ClassUpstream {
			t.Errorf("%s: class %v, want upstream", name, got)
		}
	}
}

func TestUnknownProviderNameIsUnclassified(t *testing.T) {
	if got := providers.ClassOfName("no_such_provider"); got != providers.ClassUnclassified {
		t.Fatalf("got %v", got)
	}
}
