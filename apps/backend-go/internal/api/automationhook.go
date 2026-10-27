package api

import (
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
)

// automationExecutor is the executor of a trigger_eval job: "go", the only executor since the plan 15 cutover.
func (d Deps) automationExecutor() func(ws string) string {
	return func(string) string { return config.ExecutorGo }
}

// automationHook is the row/signal/audience emitter every API write site shares: executor-routed
// (F7) with the per-signal fan-out cap INTENT_POLLER_MAX_FIRES_PER_SIGNAL.
func (d Deps) automationHook() automationhook.Emitter {
	return automationhook.NewWithOptions(d.Flags.AutomationsEnabled, d.automationExecutor(), d.Log, d.Flags.Automations.PollerMaxFiresPerSignal)
}

// AutomationHook exposes the shared emitter to cmd/api, which assigns it to Deps.AudienceEvents
// (the same emitter the worker's audience refresh uses) so the audience routes register.
func (d Deps) AutomationHook() automationhook.Emitter { return d.automationHook() }
