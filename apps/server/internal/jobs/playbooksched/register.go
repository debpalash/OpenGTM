package playbooksched

import (
	"github.com/debpalash/OpenGTM/apps/server/internal/jobs/jobkit"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

func init() {
	// Available in Go but not claimed until `opengtm routes set
	// research_playbook_schedule go`; the migration seeds no route.
	jobkit.Declare("playbooksched", JobType, register)
}

func register(env queue.Env, r *queue.Registry) error {
	NewWorker(env.Pool, env.Logger).Register(r)
	return nil
}
