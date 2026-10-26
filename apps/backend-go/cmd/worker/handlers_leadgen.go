package main

import (
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadjobs"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

// leadgenLimit is the wall-clock limit of a leadgen job: Python JOB_TIMEOUTS (source_health_check 1800).
func leadgenLimit(jobType string) (time.Duration, bool) {
	if jobType == leadjobs.JobType {
		return 1800 * time.Second, true
	}
	return 0, false
}

// leadgenOpts are test seams; the zero value is the production wiring.
type leadgenOpts struct {
	prober leadjobs.Prober     // replaces the Python worker RPC
	getenv func(string) string // replaces os.Getenv for the SOURCE_HEALTH_* knobs
}

// leadgenHandlers returns the plan 14 job handlers keyed by job type: source_health_check
// (GO_EXECUTOR_ENABLED and GO_EXECUTOR_SOURCE_HEALTH, global, no workspace cohort). Python decides
// the executor at enqueue, so Go claims executor='go' rows only; the type is always claimable and
// a row whose flag is off, or that has no Python worker to probe through, is handed back to Python
// (never failed as unsupported). The next occurrence's executor follows the flags at run time.
func leadgenHandlers(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, py *pyworker.Client, o leadgenOpts) map[string]runner {
	queue.AllowJobType(leadjobs.JobType)
	handlers := map[string]runner{
		leadjobs.JobType: handBackInert(pool, "GO_EXECUTOR_SOURCE_HEALTH is off"),
	}
	if cfg.Executor.SourceHealthExecutor() != "go" {
		return handlers
	}
	var prober leadjobs.Prober = o.prober
	if prober == nil && py != nil {
		prober = py
	}
	if prober == nil {
		log.Warn("source_health_check jobs are handed back to Python: no Python worker (PY_WORKER_URL)")
		handlers[leadjobs.JobType] = handBackInert(pool, "python worker unavailable")
		return handlers
	}
	get := o.getenv
	if get == nil {
		get = os.Getenv
	}
	handlers[leadjobs.JobType] = leadjobs.Handler(leadjobs.Deps{Pool: pool, Py: prober, Getenv: get,
		Executor: cfg.Executor.SourceHealthExecutor, Log: log})
	return handlers
}
