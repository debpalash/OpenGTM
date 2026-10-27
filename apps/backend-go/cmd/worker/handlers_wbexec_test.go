package main

import (
	"log/slog"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// GO_EXECUTOR_FULL_RUN (plan 15 WP-WBEXEC) gives the run_workbook executor its output
// clients and verify cascade; off by default (the executor keeps handing such jobs back).
func TestWireFullRun(t *testing.T) {
	var d workbook.Deps
	if err := wireFullRun(&d, config.Config{}, nil, slog.Default(), nil); err != nil || d.FullRun || d.Output != nil || d.Verify != nil {
		t.Fatalf("default must stay off: %+v %v", d, err)
	}
	t.Setenv("GO_EXECUTOR_FULL_RUN", "1")
	t.Setenv("PROVENANCE_TRACKING_ENABLED", "true")
	if err := wireFullRun(&d, config.Config{}, nil, slog.Default(), nil); err != nil || !d.FullRun || !d.Provenance || d.Output == nil || d.Verify == nil {
		t.Fatalf("flag on: %+v %v", d, err)
	}
	// review L1/L2: a malformed switch never stops the worker and reads as off
	for _, env := range []map[string]string{
		{"GO_EXECUTOR_FULL_RUN": "maybe", "PROVENANCE_TRACKING_ENABLED": "1"},
		{"GO_EXECUTOR_FULL_RUN": "", "PROVENANCE_TRACKING_ENABLED": "maybe"},
		{"GO_EXECUTOR_FULL_RUN": "0", "PROVENANCE_TRACKING_ENABLED": "maybe"},
	} {
		for k, v := range env {
			t.Setenv(k, v)
		}
		var d2 workbook.Deps
		if err := wireFullRun(&d2, config.Config{}, nil, slog.Default(), nil); err != nil || d2.FullRun || d2.Output != nil {
			t.Fatalf("%v must stay off without failing: %+v %v", env, d2, err)
		}
	}
	t.Setenv("GO_EXECUTOR_FULL_RUN", "1")
	t.Setenv("PROVENANCE_TRACKING_ENABLED", "maybe")
	var d3 workbook.Deps
	if err := wireFullRun(&d3, config.Config{}, nil, slog.Default(), nil); err != nil || !d3.FullRun || d3.Provenance {
		t.Fatalf("malformed provenance reads as off while full run is on: %+v %v", d3, err)
	}
}
