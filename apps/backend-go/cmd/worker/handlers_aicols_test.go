package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// The in-process AI columns are always wired since the plan 15 cutover (GO_NATIVE_AI_COLUMNS is retired), with web
// search/page fetch use only configured browser or opt-in API providers; without shared settings nothing is wired.
func TestWireAICols(t *testing.T) {
	cfg := config.Config{SecretKey: "a-strong-secret-key-of-32-bytes-or-more", AppEnv: "development"}
	sh, err := newWorkerShared(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	var d0 workbook.Deps
	if r := wireAICols(&d0, cfg, nil, slog.Default(), nil); r != nil || d0.AICols != nil {
		t.Fatal("wired without shared settings")
	}
	t.Setenv("GO_NATIVE_AI_COLUMNS", "false") // retired: ignored
	t.Setenv("BROWSER_SERVICE_URL", "")
	var d workbook.Deps
	if r := wireAICols(&d, cfg, nil, slog.Default(), sh); r == nil || d.AICols == nil {
		t.Fatal("the native researcher and AI columns must be wired")
	}
	ad := d.AICols.For(context.Background(), "ws-1")
	if ad.LLM == nil || ad.Settings == nil || ad.URLCheck == nil || ad.Search != nil || ad.Pages != nil {
		t.Fatalf("deps without browser service: %+v", ad)
	}
	if err := ad.URLCheck("http://169.254.169.254/latest"); err == nil {
		t.Fatal("model-chosen metadata URL must be blocked")
	}
	t.Setenv("BROWSER_SERVICE_URL", "http://browser:8080")
	t.Setenv("BROWSER_SERVICE_TOKEN", "tok")
	var d2 workbook.Deps
	wireAICols(&d2, cfg, nil, slog.Default(), sh)
	if ad := d2.AICols.For(context.Background(), "ws-1"); ad.Search == nil || ad.Pages == nil {
		t.Fatalf("deps with browser service: %+v", ad)
	}
}

func TestWireAIColsWithFirecrawlWithoutBrowser(t *testing.T) {
	for _, key := range []string{"BROWSER_SERVICE_URL", "BROWSER_SERVICE_TOKEN", "SERPAPI_KEY", "BING_SEARCH_KEY",
		"GOOGLE_CSE_KEY", "GOOGLE_CSE_ID", "BRAVE_SEARCH_KEY", "SERPINGAPI_API_KEY", "SERPLY_API_KEY"} {
		t.Setenv(key, "")
	}
	t.Setenv("FIRECRAWL_API_KEY", "fc-wire-test")
	cfg := config.Config{SecretKey: "a-strong-secret-key-of-32-bytes-or-more", AppEnv: "test"}
	shared, err := newWorkerShared(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	var deps workbook.Deps
	wireAICols(&deps, cfg, nil, slog.Default(), shared)
	if deps.AICols == nil {
		t.Fatal("research columns are not wired")
	}
	forWorkspace := deps.AICols.For(context.Background(), "firecrawl-only")
	if forWorkspace.Search == nil || forWorkspace.Pages != nil {
		t.Fatal("API-only search must be available without enabling an unconfigured page fetcher")
	}
}

func TestWireAIColsWithSpicrawlWithoutBrowser(t *testing.T) {
	for _, key := range []string{"BROWSER_SERVICE_URL", "BROWSER_SERVICE_TOKEN", "SERPAPI_KEY", "BING_SEARCH_KEY",
		"GOOGLE_CSE_KEY", "GOOGLE_CSE_ID", "BRAVE_SEARCH_KEY", "SERPINGAPI_API_KEY", "SERPLY_API_KEY", "FIRECRAWL_API_KEY"} {
		t.Setenv(key, "")
	}
	t.Setenv("SPICRAWL_API_KEY", "sp-wire-test")
	cfg := config.Config{SecretKey: "a-strong-secret-key-of-32-bytes-or-more", AppEnv: "test"}
	shared, err := newWorkerShared(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	var deps workbook.Deps
	wireAICols(&deps, cfg, nil, slog.Default(), shared)
	if deps.AICols == nil {
		t.Fatal("research columns are not wired")
	}
	configured := deps.AICols.For(context.Background(), "spicrawl-only")
	if configured.Pages == nil || configured.Search != nil {
		t.Fatal("markdown-only configuration not respected")
	}
	t.Setenv("FIRECRAWL_API_KEY", "fc-with-spicrawl")
	var both workbook.Deps
	wireAICols(&both, cfg, nil, slog.Default(), shared)
	combined := both.AICols.For(context.Background(), "both-api-providers")
	if combined.Pages == nil || combined.Search == nil {
		t.Fatal("API-only research must expose search and page fetch")
	}
}
