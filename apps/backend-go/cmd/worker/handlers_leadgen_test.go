package main

import (
	"io"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
)

// source_health_check always has a handler (a hand-back while its flag is off).
func TestLeadgenHandlersAlwaysRegistered(t *testing.T) {
	log := telemetry.NewLogger(io.Discard)
	for _, on := range []bool{false, true} {
		cfg := config.Config{Executor: config.ExecutorSettings{Enabled: on, SourceHealth: on}}
		if got := leadgenHandlers(cfg, nil, log, nil, leadgenOpts{}); got["source_health_check"] == nil {
			t.Errorf("flag %v: source_health_check not registered", on)
		}
	}
}

func TestLeadgenJobLimit(t *testing.T) {
	d, ok := leadgenLimit("source_health_check")
	if !ok || int(d.Seconds()) != 1800 {
		t.Fatalf("%v %v", d, ok)
	}
	if _, ok := leadgenLimit("send"); ok {
		t.Fatal("send is not a leadgen job")
	}
}
