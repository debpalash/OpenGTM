package workbook

import "fmt"

// Capabilities a run_workbook job can require beyond the default executor (plan 15 review M4).
// The enqueuer lists them in the payload's go block ("requires"); a worker that lacks one hands
// the job back before any work instead of silently running without provenance, automation
// events, conditions, email verification or output delivery.
const (
	CapProvenance  = "provenance"
	CapAutomations = "automations"
	CapConditions  = "conditions"
	CapEmailVerify = "email_verify"
	CapOutput      = "output"
)

// checkRequires refuses (hands back) a job that needs a capability this worker was not wired
// with. An unknown name fails closed: a newer enqueuer may require something this build lacks.
func checkRequires(d Deps, requires []string) error {
	for _, c := range requires {
		var have bool
		switch c {
		case CapProvenance:
			have = d.FullRun && d.Provenance
		case CapAutomations:
			have = d.FullRun && d.Emitter != nil
		case CapConditions:
			have = d.FullRun
		case CapEmailVerify:
			have = d.FullRun && d.Verify != nil
		case CapOutput:
			have = d.FullRun && d.Output != nil
		}
		if !have {
			return refuse("capability_unavailable: this worker lacks %s", fmt.Sprintf("%q", c))
		}
	}
	return nil
}
