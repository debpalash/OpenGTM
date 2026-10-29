package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/webui"
)

// S-M3: the embedded UI is the catch-all for GET/HEAD; every route prefix the API registers must be
// excluded from it, or the SPA index.html answers instead of the API (e.g. an unknown /scim path).
func TestEveryRoutePrefixIsExcludedFromWebUI(t *testing.T) {
	cfg := config.APIConfig{SecretKey: strings.Repeat("k", 32), AppEnv: "test"}
	r, err := buildRouter(cfg, telemetry.NewLogger(io.Discard), nil, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pats := r.Patterns()
	if len(pats) < 50 {
		t.Fatalf("suspiciously few patterns: %d", len(pats))
	}
	for _, p := range pats {
		_, path, ok := strings.Cut(p, " ")
		if !ok {
			t.Fatalf("bad pattern %q", p)
		}
		// Host-qualified patterns have no leading slash before the host; skip none: all start with "/".
		seg := path
		if i := strings.IndexAny(seg[1:], "/{"); i >= 0 {
			seg = seg[:i+1]
		}
		if !webui.Excluded(seg + "/x") {
			t.Errorf("route %q: prefix %q is not excluded from the web UI", p, seg)
		}
	}
}
