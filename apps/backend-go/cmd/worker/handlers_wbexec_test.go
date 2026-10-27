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
	t.Setenv("GO_EXECUTOR_FULL_RUN", "maybe")
	if err := wireFullRun(&d, config.Config{}, nil, slog.Default(), nil); err == nil {
		t.Fatal("a malformed flag must stop the worker")
	}
}
