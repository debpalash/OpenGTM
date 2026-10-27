package api

import (
	"testing"
)

// GO_EXECUTOR_FULL_RUN gives the single-cell route its own output clients and verify
// cascade, so it never proxies to PYTHON_API_URL; off by default.
func TestCellDepsFullRun(t *testing.T) {
	off := cellDeps(Deps{}, nil)
	if off.FullRun || off.Output != nil || off.Verify != nil {
		t.Fatalf("defaults must keep the proxy: %+v", off)
	}
	on := cellDeps(Deps{Getenv: func(k string) string {
		switch k {
		case "GO_EXECUTOR_FULL_RUN", "PROVENANCE_TRACKING_ENABLED":
			return "true"
		}
		return ""
	}}, nil)
	if !on.FullRun || !on.Provenance || on.Output == nil || on.Verify == nil || on.Client == nil {
		t.Fatalf("full run wiring: %+v", on)
	}
}
