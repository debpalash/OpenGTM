package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// The in-process AI columns are always wired since the plan 15 cutover (GO_NATIVE_AI_COLUMNS is retired), with web
// search and page fetch only when the browser service is configured; without shared settings nothing is wired.
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
