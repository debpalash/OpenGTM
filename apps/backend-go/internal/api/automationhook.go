package api

import (
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
)

// automationExecutor is go_executor.trigger_eval_executor: "go" only when GO_EXECUTOR_ENABLED,
// GO_EXECUTOR_TRIGGER_EVAL and the workspace is in GO_EXECUTOR_WORKSPACES. The cohort is read
// from the environment per call, like the refresh executor, so a flag change applies at once.
func (d Deps) automationExecutor() func(ws string) string {
	return func(ws string) string {
		c := config.ExecutorCohortFrom(d.getenv())
		return automationhook.CohortExecutor(c.Enabled, d.Flags.Automations.GoExecutorTriggerEval, c.Workspaces)(ws)
	}
}

// automationHook is the row/signal/audience emitter every API write site shares: executor-routed
// (F7) with the per-signal fan-out cap INTENT_POLLER_MAX_FIRES_PER_SIGNAL.
func (d Deps) automationHook() automationhook.Emitter {
	return automationhook.NewWithOptions(d.Flags.AutomationsEnabled, d.automationExecutor(), d.Log, d.Flags.Automations.PollerMaxFiresPerSignal)
}

// AutomationHook exposes the shared emitter to cmd/api, which assigns it to Deps.AudienceEvents
// (the same emitter the worker's audience refresh uses) so the audience routes register.
func (d Deps) AutomationHook() automationhook.Emitter { return d.automationHook() }
