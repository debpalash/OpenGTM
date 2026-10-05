package main

import (
	"context"

	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"github.com/debpalash/OpenGTM/apps/server/internal/db"
	_ "github.com/debpalash/OpenGTM/apps/server/internal/jobs/retention" // registers retention_enforce (switchable, not routed by default)
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

func init() {
	register("worker", "run the durable queue worker for job types routed to go", runWorker)
}

// Handlers are contributed by domain packages through queue.AddRegistrar in
// their init(); importing such a package into this binary is enough to make
// `opengtm worker` claim its job types (once they are routed to go).
func runWorker(ctx context.Context, args []string) error {
	fs, cfgPath := roleFlags("worker")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	log := roleLogger(cfg, "worker")
	// Each busy slot holds a claim/finalize connection and its lease monitor
	// may need another; the reaper and metrics share the remainder.
	pool, err := db.Open(ctx, cfg.DatabaseURL, db.Options{
		AppName:  "opengtm-worker",
		MaxConns: int32(4 + 2*cfg.Worker.Concurrency),
	})
	if err != nil {
		return err
	}
	defer pool.Close()
	return queue.RunWorker(ctx, queue.Env{Pool: pool, Config: cfg, Logger: log})
}
