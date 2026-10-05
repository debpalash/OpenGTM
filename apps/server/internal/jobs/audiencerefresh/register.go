package audiencerefresh

import (
	"github.com/debpalash/OpenGTM/apps/server/internal/jobs/jobkit"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

func init() {
	// Available in Go but not claimed until `opengtm routes set
	// audience_refresh go`; the migration seeds no route.
	jobkit.Declare("audiencerefresh", JobType, register)
}

func register(env queue.Env, r *queue.Registry) error {
	NewWorker(env.Pool, env.Logger, Options{AutomationsEnabled: env.Config.Automations.Enabled}).Register(r)
	return nil
}
