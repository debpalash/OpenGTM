package api

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
)

// GO_EXECUTOR_FULL_RUN gives the single-cell route its own output clients and verify
// cascade; retired at the plan 15 cutover, so they are always wired.
func TestCellDepsFullRun(t *testing.T) {
	off := cellDeps(Deps{}, nil)
	if !off.FullRun || off.Output == nil || off.Verify == nil || off.Py != nil {
		t.Fatalf("defaults must run every column in process: %+v", off)
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

// GO_NATIVE_AI_COLUMNS (plan 15 WP-AICOLS) wires the in-process AI column path into the
// single-cell route; retired at the cutover, so it is on whatever the environment says.
func TestCellDepsNativeAICols(t *testing.T) {
	reg, err := settings.Load()
	if err != nil {
		t.Fatal(err)
	}
	env, err := secrets.NewEnvelope(secrets.Config{MasterKey: "k", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	get := func(on string) func(string) string {
		return func(k string) string {
			if k == "GO_NATIVE_AI_COLUMNS" {
				return on
			}
			return ""
		}
	}
	if d := cellDeps(Deps{Getenv: get(""), Registry: reg, Envelope: env}, nil); d.AICols == nil {
		t.Fatal("AI columns must run in process by default")
	}
	d := cellDeps(Deps{Getenv: get("1"), Registry: reg, Envelope: env}, nil)
	if d.AICols == nil || d.AICols.For == nil {
		t.Fatal("GO_NATIVE_AI_COLUMNS=1 must wire the native path")
	}
	if d.AICols.For(t.Context(), "ws").LLM == nil {
		t.Fatal("per-workspace deps need an LLM client")
	}
	if d := cellDeps(Deps{Getenv: get("1")}, nil); d.AICols != nil {
		t.Fatal("without settings and envelope the path cannot be wired")
	}
}
