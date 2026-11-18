//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/mcp"
)

// Issue 114 closure (MCP production wiring), miss path: a provider that finds nothing for a lead (website_scraper on a
// site without contact data) is a miss in the field waterfall, not a failure of the tool. enrich_company documents
// "per field the first valid value wins" over each chain; the answer for a lead nothing could enrich is an empty
// enriched_fields, as the mocked-port test TestEnrichCompanyResultShape expects for an empty provider answer.
func TestEnrichCompanyProviderMissIsNotAToolFailureThroughProductionRuntime(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	browser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 200, "engine": "impersonate", "html": `<html><body><h1>Nothing here</h1></body></html>`})
	}))
	t.Cleanup(browser.Close)
	env := map[string]string{"DATABASE_URL": appURL(t), "APP_ENV": "test", "BROWSER_SERVICE_URL": browser.URL, "BROWSER_SERVICE_TOKEN": "t114-browser-token"}
	get := func(k string) string { return env[k] }
	cfg, err := config.LoadMCPFrom(get)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := openRuntime(ctx, cfg, get)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.close)
	// s.leads[2] ("Éco Solutions") has a website and no email, phone or LinkedIn URL.
	raw := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"enrich_company","arguments":{"lead_id":%d}}}`, s.leads[2])
	resp, _ := rt.srv.Handle(ctx, mcp.Ctx{WorkspaceID: s.ws, Capabilities: map[string]bool{"leads:read": true}}, []byte(raw))
	var r struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &r); err != nil || len(r.Result.Content) != 1 {
		t.Fatalf("bad response %s: %v", resp, err)
	}
	if got := r.Result.Content[0].Text; strings.Contains(got, "enrichment failed") || !strings.Contains(got, `"enriched_fields"`) {
		t.Fatalf("a provider miss failed the tool: %s", got)
	}
}
