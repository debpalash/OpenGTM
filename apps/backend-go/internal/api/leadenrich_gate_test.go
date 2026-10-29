package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// gatedSpy is a GO_LOCAL_PROVIDERS-gated provider that records the workspace on its ctx.
type gatedSpy struct {
	name string
	ws   *[]string
}

func (g gatedSpy) Name() string { return g.name }
func (gatedSpy) LocalGated()    {}
func (g gatedSpy) Enrich(ctx context.Context, _ *http.Client, _ func() (string, error), _ providers.Input) (providers.Output, error) {
	*g.ws = append(*g.ws, providers.WorkspaceFrom(ctx))
	return providers.Output{Extra: providers.NewObj()}, nil
}

// L8: lead-enrich's find_emails / scrape_website respect GO_LOCAL_PROVIDERS for gated providers and
// salt the providers' search cache with the calling workspace.
func TestLeadEnrichGatedProviders(t *testing.T) {
	t.Cleanup(func() { providers.SetLocalGate(nil) })
	var seen []string
	store := &fakeLeadStore{leads: map[int64]*leadsread.Lead{1: leadFrom(t, map[string]any{"id": 1, "company": "C", "website": "https://example.com"})}}
	h := newTestRoutes(store, &scriptLLM{})
	h.providerLookup = mapProviders{"ddg_email": gatedSpy{"ddg_email", &seen}, "website_scraper": gatedSpy{"website_scraper", &seen}}
	run := func(action string) string {
		rec := httptest.NewRecorder()
		e := &enrichRun{h: h, ctx: context.Background(), ws: "ws-a", lead: store.leads[1], out: sseOut{w: rec}}
		e.do(action)
		return rec.Body.String()
	}
	providers.SetLocalGate(nil)
	if body := run("find_emails"); !strings.Contains(body, "Email finder is not available") || len(seen) != 0 {
		t.Fatalf("gate off ran the gated email finder: %q %v", body, seen)
	}
	if body := run("scrape_website"); !strings.Contains(body, "Website scraper is not available") || len(seen) != 0 {
		t.Fatalf("gate off ran the gated scraper: %q %v", body, seen)
	}
	providers.SetLocalGate(func(string) bool { return true })
	run("find_emails")
	run("scrape_website")
	if len(seen) != 2 || seen[0] != "ws-a" || seen[1] != "ws-a" {
		t.Fatalf("provider ctx workspaces %q, want the caller's ws-a twice", seen)
	}
}
