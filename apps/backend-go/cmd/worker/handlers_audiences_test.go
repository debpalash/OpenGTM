package main

import (
	"context"
	"io"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destdeliver"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
)

// Per-type wall-clock limits: Python JOB_TIMEOUTS (audience_refresh 900, audience_destination_sync 1800).
func TestWorkerAudienceTimeoutsPerType(t *testing.T) {
	for typ, secs := range map[string]int{"audience_refresh": 900, "audience_destination_sync": 1800} {
		d, ok := audienceLimit(typ)
		if !ok || int(d.Seconds()) != secs {
			t.Errorf("%s: %v %v, want %ds", typ, d, ok, secs)
		}
		if got := jobLimit(config.Config{JobTimeout: 1234}, typ); int(got.Seconds()) != secs {
			t.Errorf("jobLimit(%s) = %v, want %ds", typ, got, secs)
		}
	}
	if _, ok := audienceLimit("send"); ok {
		t.Error("send is not an audience job")
	}
}

// Both types always have a handler (a hand-back while off or unwired), whatever the flags and
// whether or not an emitter exists, so a go row is never failed as an unsupported type.
func TestAudienceHandlersAlwaysRegistered(t *testing.T) {
	log := telemetry.NewLogger(io.Discard)
	for _, on := range []bool{false, true} {
		cfg := config.Config{Flags: config.Flags{Audiences: config.AudienceSettings{GoExecutorAudienceRefresh: on}}}
		got := audienceHandlers(cfg, nil, log, nil, nil, audienceOpts{})
		for _, k := range []string{"audience_refresh", "audience_destination_sync"} {
			if got[k] == nil {
				t.Errorf("flag %v: %s not registered", on, k)
			}
		}
	}
}

// A resolver that cannot say where a secret came from (CREDENTIALS_SOURCE=http) cannot serve the
// workspace-only types, so those rows go back to Python instead of failing every delivery.
func TestProvenanceEligible(t *testing.T) {
	cases := []struct {
		sec  destdeliver.SecretResolver
		typ  string
		want bool
	}{
		{apiSecrets{}, "warehouse_http", false},
		{apiSecrets{}, "slack", false},
		{apiSecrets{}, "meta_ads", true},
		{apiSecrets{}, "webhook", true},
		{fakeResolver{}, "warehouse_http", true},
		{nil, "warehouse_http", true},
	}
	for _, c := range cases {
		if got := provenanceEligible(c.sec, c.typ); got != c.want {
			t.Errorf("%T %s: got %v want %v", c.sec, c.typ, got, c.want)
		}
	}
}

type fakeResolver struct{}

func (fakeResolver) Get(context.Context, string, string) (string, string, error) { return "", "", nil }

type srcFake struct{ source string }

func (s srcFake) Get(context.Context, string, string) (string, string, error) {
	return "tok", s.source, nil
}

// S-L25: AD_TOKENS_WORKSPACE_ONLY off keeps the global/env fallback; on, a global-only token reads as unset
// (delivery fails "missing workspace secret ..."), and an http-credentials resolver hands ad types back.
func TestAdTokenGetterWorkspaceOnly(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		only   bool
		source string
		want   string
	}{{false, "global", "tok"}, {false, "env", "tok"}, {true, "global", ""}, {true, "env", ""}, {true, "workspace", "tok"}} {
		got, err := adTokenGetter(srcFake{tc.source}, tc.only)(ctx, "ws", "META_ACCESS_TOKEN")
		if err != nil || got != tc.want {
			t.Errorf("only=%v %s: %q %v", tc.only, tc.source, got, err)
		}
	}
	t.Setenv("AD_TOKENS_WORKSPACE_ONLY", "true")
	if provenanceEligible(apiSecrets{}, "meta_ads") || !provenanceEligible(fakeResolver{}, "meta_ads") {
		t.Error("ad types need a provenance-aware resolver when tokens are workspace-only")
	}
}
