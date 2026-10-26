package main

import (
	"io"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
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
