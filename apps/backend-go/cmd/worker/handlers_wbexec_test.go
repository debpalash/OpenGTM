package main

import (
	"log/slog"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// The run_workbook executor always gets its output clients and verify cascade since the plan 15 cutover
// (GO_EXECUTOR_FULL_RUN is retired); a malformed PROVENANCE_TRACKING_ENABLED reads as off and never stops it.
func TestWireFullRun(t *testing.T) {
	t.Setenv("GO_EXECUTOR_FULL_RUN", "0") // retired: ignored
	var d workbook.Deps
	if err := wireFullRun(&d, config.Config{}, nil, slog.Default(), nil); err != nil || !d.FullRun || d.Provenance || d.Output == nil || d.Verify == nil {
		t.Fatalf("default: %+v %v", d, err)
	}
	t.Setenv("PROVENANCE_TRACKING_ENABLED", "true")
	var d2 workbook.Deps
	if err := wireFullRun(&d2, config.Config{}, nil, slog.Default(), nil); err != nil || !d2.FullRun || !d2.Provenance {
		t.Fatalf("provenance on: %+v %v", d2, err)
	}
	t.Setenv("PROVENANCE_TRACKING_ENABLED", "maybe")
	var d3 workbook.Deps
	if err := wireFullRun(&d3, config.Config{}, nil, slog.Default(), nil); err != nil || !d3.FullRun || d3.Provenance {
		t.Fatalf("malformed provenance reads as off: %+v %v", d3, err)
	}
}
