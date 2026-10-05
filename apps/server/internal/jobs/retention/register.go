package retention

import "github.com/debpalash/OpenGTM/apps/server/internal/queue"

func init() {
	queue.AddRegistrar("retention", register)
	// Available in Go but not claimed until `opengtm routes set
	// retention_enforce go`; the migration deliberately seeds no route.
	queue.DeclareSwitchable(JobType)
}

func register(env queue.Env, r *queue.Registry) error {
	NewWorker(env.Pool, env.Logger).Register(r)
	return nil
}
