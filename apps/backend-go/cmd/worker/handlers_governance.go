package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/playbooks"
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
	researcher playbooks.Researcher // the in-process researcher (AI columns); nil fails playbook runs
	afterBatch func(string, int64)  // retention Deps.AfterBatch
	batchRows  int                  // retention Deps.BatchRows
}

// governanceHandlers returns the plan 11 job handlers keyed by job type: retention_enforce and
// research_playbook_run / research_playbook_schedule, all registered unconditionally since the plan 15 cutover
// (GO_EXECUTOR_RETENTION and GO_EXECUTOR_PLAYBOOKS are retired). A playbook run without a research runner (the AI
// columns could not be wired) fails permanently. Failure hooks (retention_enforce and research_playbook_run only:
// Python registered none for research_playbook_schedule) reconcile the run after a persisted failure (handler
// error or recovered panic) with willRetry from the job row. The Go playbook run enforces the budget itself.
func governanceHandlers(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, o governanceOpts) map[string]runner {
	queue.AllowJobType(retention.JobType)
	queue.AllowJobType(playbooks.RunJobType)
	queue.AllowJobType(playbooks.ScheduleJobType)
	rd := retention.Deps{Pool: pool, BatchRows: o.batchRows, AfterBatch: o.afterBatch}
	retention.RegisterFailureHook(rd)
	ret := retention.Handler(rd)
	pd := playbooks.Deps{Pool: pool}
	if o.researcher == nil {
		log.Warn("research_playbook_run jobs will fail: no research runner (the AI column LLM settings could not be wired)")
	}
	playbooks.RegisterFailureHook(pd)
	run, sched := playbooks.RunHandler(pd, o.researcher), playbooks.ScheduleHandler(pd)
	return map[string]runner{
		retention.JobType:         func(ctx context.Context, c *queue.Claim) error { return ret(ctx, c) },
		playbooks.RunJobType:      func(ctx context.Context, c *queue.Claim) error { return run(ctx, c) },
		playbooks.ScheduleJobType: func(ctx context.Context, c *queue.Claim) error { return sched(ctx, c) },
	}
}
