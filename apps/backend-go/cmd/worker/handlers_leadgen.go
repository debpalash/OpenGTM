package main

import (
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collect"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadjobs"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

// leadgenLimit is the wall-clock limit of a leadgen job: Python JOB_TIMEOUTS (source_health_check 1800).
func leadgenLimit(jobType string) (time.Duration, bool) {
	if jobType == leadjobs.JobType {
		return 1800 * time.Second, true
	}
	return 0, false
}

// leadgenOpts are the source_health_check collaborators; the zero value fails the job.
type leadgenOpts struct {
	prober leadjobs.Prober     // the in-process registry-source prober (nativeProber); nil: the job fails
	why    string              // why prober is nil (logged and in the job error)
	getenv func(string) string // replaces config.Getenv for the SOURCE_HEALTH_* knobs
}

// nativeProber probes the registry sources in process over the worker's leadweb (GO_NATIVE_SOURCE_PROBE is
// permanently on since the plan 15 cutover); nil without a leadweb (no browser service).
func nativeProber(web *leadweb.Web) leadjobs.Prober {
	if web == nil {
		return nil
	}
	return collect.NativeProber{Web: collect.LiveWeb{W: web}}
}

// leadgenHandlers returns the plan 14 job handler source_health_check (global, no workspace). It probes in process
// (there is no Python worker since the cutover); without a prober the job fails permanently with the reason.
func leadgenHandlers(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, o leadgenOpts) map[string]runner {
	queue.AllowJobType(leadjobs.JobType)
	if o.prober == nil {
		why := o.why
		if why == "" {
			why = "no source prober (the browser service is not configured)"
		}
		log.Warn("source_health_check jobs will fail", "reason", why)
		return map[string]runner{leadjobs.JobType: refuseJob(leadjobs.JobType, why)}
	}
	get := o.getenv
	if get == nil {
		get = config.Getenv
	}
	return map[string]runner{leadjobs.JobType: leadjobs.Handler(leadjobs.Deps{Pool: pool, Py: o.prober, Getenv: get, Log: log})}
}
