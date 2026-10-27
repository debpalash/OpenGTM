package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// GO_NATIVE_AI_COLUMNS (plan 15 WP-AICOLS): off keeps the Python worker paths (no in-process
// AI columns, no native researcher); on wires both, with web search and page fetch only when
// the browser service is configured.
func TestWireAICols(t *testing.T) {
	cfg := config.Config{SecretKey: "a-strong-secret-key-of-32-bytes-or-more", AppEnv: "development"}
	var d workbook.Deps
	if r := wireAICols(&d, cfg, nil, slog.Default()); r != nil || d.AICols != nil {
		t.Fatalf("default must stay off: %v %+v", r, d.AICols)
	}
	t.Setenv("GO_NATIVE_AI_COLUMNS", "maybe")
	if r := wireAICols(&d, cfg, nil, slog.Default()); r != nil || d.AICols != nil {
		t.Fatal("a malformed switch reads as off")
	}
	t.Setenv("GO_NATIVE_AI_COLUMNS", "1")
	t.Setenv("BROWSER_SERVICE_URL", "")
	if r := wireAICols(&d, cfg, nil, slog.Default()); r == nil || d.AICols == nil {
		t.Fatal("flag on must wire the native researcher and AI columns")
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
	wireAICols(&d2, cfg, nil, slog.Default())
	if ad := d2.AICols.For(context.Background(), "ws-1"); ad.Search == nil || ad.Pages == nil {
		t.Fatalf("deps with browser service: %+v", ad)
	}
}
