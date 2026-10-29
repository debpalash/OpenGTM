package main

import (
	"io"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
)

// source_health_check always has a handler (plan 15 cutover: GO_EXECUTOR_SOURCE_HEALTH is retired).
func TestLeadgenHandlersAlwaysRegistered(t *testing.T) {
	log := telemetry.NewLogger(io.Discard)
	if got := leadgenHandlers(config.Config{}, nil, log, leadgenOpts{}); got["source_health_check"] == nil {
		t.Error("source_health_check not registered")
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
