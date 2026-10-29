package main

import (
	"context"
	"io"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/runplan"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
)

// TestStartupWiresRunPlan: runplan.Wire (vendor catalog, research flag and the
// billing cost catalog) must have run by the time the router is built; without
// it a run is projected with the research-only default catalog.
func TestStartupWiresRunPlan(t *testing.T) {
	runplan.ResetWiredForTest()
	if runplan.Wired() {
		t.Fatal("reset failed")
	}
	if _, err := buildRouter(config.APIConfig{SecretKey: "k"}, telemetry.NewLogger(io.Discard), nil, context.Background()); err != nil {
		t.Fatal(err)
	}
	if !runplan.Wired() {
		t.Fatal("buildRouter did not call runplan.Wire")
	}
}
