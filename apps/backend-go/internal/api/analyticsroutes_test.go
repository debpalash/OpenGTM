package api

import (
	"io"
	"log/slog"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

func TestAnalyticsNotRegisteredWithoutPGStore(t *testing.T) {
	d := Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Flags: config.Flags{PGLeadStore: false}}
	r := httpx.NewRouter()
	RegisterAnalytics(r, d)
	if n := len(r.Patterns()); n != 0 {
		t.Fatalf("%d routes registered without PG_LEAD_STORE", n)
	}
}
