package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/playbooks"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/retention"
)

// governanceLimit is the wall-clock limit of a governance job: Python JOB_TIMEOUTS
// (retention_enforce 1800, research_playbook_run 3600, research_playbook_schedule 300).
func governanceLimit(jobType string) (time.Duration, bool) {
	switch jobType {
	case retention.JobType:
		return 1800 * time.Second, true
	case playbooks.RunJobType:
		return 3600 * time.Second, true
	case playbooks.ScheduleJobType:
		return 300 * time.Second, true
	}
	return 0, false
}

// governanceOpts are test seams; the zero value is the production wiring.
type governanceOpts struct {
	researcher playbooks.Researcher // replaces the Python-worker researcher
	afterBatch func(string, int64)  // retention Deps.AfterBatch
	batchRows  int                  // retention Deps.BatchRows
}

// governanceHandlers returns the plan 11 job handlers keyed by job type: retention_enforce
// (GO_EXECUTOR_RETENTION) and research_playbook_run / research_playbook_schedule
// (GO_EXECUTOR_PLAYBOOKS). Python decides the executor at enqueue (cohort + per-type flag),
// so Go claims by type on executor='go' rows only; a type whose flag is off is handed back. A run job whose Python worker is not configured, or becomes
// unavailable, is handed back to Python by the handler. Failure hooks (retention_enforce and research_playbook_run only: Python registers none for
// research_playbook_schedule) reconcile the run
// after a persisted failure (handler error or recovered panic) with willRetry from the job
// row. Budget note: the Python research_column RPC reports cost_usd 0, so the Go playbook
// run enforces the budget itself.
func governanceHandlers(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, py *pyworker.Client, o governanceOpts) map[string]runner {
	// Every governance type is always claimable: an executor='go' row that reaches this
	// worker while its flag is off is handed back to Python, not failed as unsupported.
	queue.AllowJobType(retention.JobType)
	queue.AllowJobType(playbooks.RunJobType)
	queue.AllowJobType(playbooks.ScheduleJobType)
	handlers := map[string]runner{
		retention.JobType:         handBackInert(pool, "GO_EXECUTOR_RETENTION is off"),
		playbooks.RunJobType:      handBackInert(pool, "GO_EXECUTOR_PLAYBOOKS is off"),
		playbooks.ScheduleJobType: handBackInert(pool, "GO_EXECUTOR_PLAYBOOKS is off"),
	}
	gov := cfg.Flags.Governance
	if gov.GoExecutorRetention {
		d := retention.Deps{Pool: pool, BatchRows: o.batchRows, AfterBatch: o.afterBatch,
			ExecutorFor: func(ws string) string { return retention.ExecutorFor(cfg.Executor, gov, ws) }}
		retention.RegisterFailureHook(d)
		h := retention.Handler(d)
		handlers[retention.JobType] = func(ctx context.Context, c *queue.Claim) error { return h(ctx, c) }
	}
	if gov.GoExecutorPlaybooks {
		playbooks.WireExecutors(cfg.Executor, gov)
		d := playbooks.Deps{Pool: pool, RunExecutor: func(ws string) string { return playbooks.ExecutorFor(cfg.Executor, gov, ws) }}
		var res playbooks.Researcher = o.researcher
		if res == nil && py != nil {
			res = playbooks.NewPyResearcher(py, playbooks.StepTimeout())
		}
		if res == nil {
			log.Warn("playbook run jobs are handed back to Python: no Python worker (PY_WORKER_URL)")
		}
		playbooks.RegisterFailureHook(d)
		run, sched := playbooks.RunHandler(d, res), playbooks.ScheduleHandler(d)
		handlers[playbooks.RunJobType] = func(ctx context.Context, c *queue.Claim) error { return run(ctx, c) }
		handlers[playbooks.ScheduleJobType] = func(ctx context.Context, c *queue.Claim) error { return sched(ctx, c) }
	}
	return handlers
}
