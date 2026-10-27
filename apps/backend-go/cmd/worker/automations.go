package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoactions"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoengine"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crmpush"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachsend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/poller"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// Plan 08 job types and their wall-clock limit (Python JOB_TIMEOUTS).
const (
	jobTriggerEval    = autoengine.JobType // "trigger_eval"
	jobWatchPoll      = "watch_poll"
	automationTimeout = 600 * time.Second
)

// triggerEvalExecutor / watchPollExecutor are go_executor.trigger_eval_executor and
// watch_poll_executor: the executor of a job this worker enqueues or reschedules.
func triggerEvalExecutor(cfg config.Config) func(ws string) string {
	return automationhook.CohortExecutor(cfg.Executor.Enabled, cfg.Flags.Automations.GoExecutorTriggerEval, cfg.Executor.Workspaces)
}

func watchPollExecutor(cfg config.Config) func(ws string) string {
	return automationhook.CohortExecutor(cfg.Executor.Enabled, cfg.Flags.Automations.GoExecutorWatchPoll, cfg.Executor.Workspaces)
}

// automationEmitter is the one on_rows_changed/on_signal/audience emitter the worker shares
// (workbook run-end flush, poller signals, ambitionbox, signal scan). Its jobs go to the
// executor the trigger_eval cohort names.
func automationEmitter(cfg config.Config, log *slog.Logger) automationhook.Emitter {
	return automationhook.NewWithOptions(cfg.Flags.AutomationsEnabled, triggerEvalExecutor(cfg), log,
		cfg.Flags.Automations.PollerMaxFiresPerSignal)
}

// logAutomationFlags records the effective plan 08 flags once at startup. The worker cannot
// verify that they equal the API's (AUTOMATIONS_ENABLED must match on both stacks).
func logAutomationFlags(log *slog.Logger, cfg config.Config) {
	a := cfg.Flags.Automations
	log.Info("automation flags",
		"automations_enabled", cfg.Flags.AutomationsEnabled,
		"intent_poller_enabled", cfg.Flags.IntentPollerEnabled,
		"pg_lead_store", cfg.Flags.PGLeadStore,
		"go_executor_enabled", cfg.Executor.Enabled,
		"go_executor_trigger_eval", a.GoExecutorTriggerEval,
		"go_executor_watch_poll", a.GoExecutorWatchPoll,
		"allow_legacy_outreach", cfg.Flags.AutomationsAllowLegacyOutreach,
		"billing_enabled", cfg.BillingEnabled)
	// safecond reads naive timestamps in the process-local zone (Python does too): both stacks
	// must run with the same TZ.
	log.Info("process timezone", "tz", time.Local.String())
	if cfg.Flags.AutomationsAllowLegacyOutreach {
		log.Warn("AUTOMATIONS_ALLOW_LEGACY_OUTREACH is on: Python keeps the legacy outreach surface; this worker hands such trigger_eval jobs back")
	}
}

// handBackInert is the handler of a job type whose GO_EXECUTOR_* flag is off: a row that
// reached this worker anyway is returned to Python untouched.
func handBackInert(pool *pgxpool.Pool, why string) runner {
	return func(ctx context.Context, c *queue.Claim) error {
		ok, err := queue.HandBack(ctx, pool, c, why)
		if err != nil {
			return err
		}
		if !ok {
			return workbook.ErrLeaseLost
		}
		return fmt.Errorf("%w: %s", workbook.ErrHandedBack, why)
	}
}

// crmPushGuard fails push_crm closed when the Go trigger_eval executor is on but the worker has no
// secret resolver (CREDENTIALS_SOURCE=http): the CRM clients would silently use env-only tokens
// and reach the wrong account. It returns nil when no guard is needed, and logs once at startup.
func crmPushGuard(local *secrets.Resolver, evalOn bool, log *slog.Logger) autoactions.PushFunc {
	if local != nil || !evalOn {
		return nil
	}
	log.Warn("push_crm will fail closed: no secret resolver; set CREDENTIALS_SOURCE=local to run push_crm actions on the Go worker")
	return func(context.Context, string, map[string]any, map[string]any, string, int64) crmpush.Result {
		return crmpush.Result{Error: "crm credentials unavailable on this worker (requires CREDENTIALS_SOURCE=local)"}
	}
}

// buildAutomations registers trigger_eval and watch_poll (claimable, failure hook for
// watch_poll) and returns their handlers. The heartbeat and cancel watcher of processJob
// covers both handlers, including a single-source watch_poll or a long re_enrich that only
// check the lease between steps. wbDeps feeds re_enrich; sec resolves webhook header
// secrets; emitter is the shared emitter; local is the in-process secret resolver (nil:
// CRM tokens come from the environment only).
func buildAutomations(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, wbDeps workbook.Deps,
	sec autoactions.SecretGetter, local *secrets.Resolver, pyRPC *pyworker.Client, provRPC poller.PyCaller,
	emitter automationhook.Emitter) map[string]runner {
	a := cfg.Flags.Automations
	queue.AllowJobType(jobTriggerEval)
	queue.AllowJobType(jobWatchPoll)

	// The poller core, HTTP sources and pyworker collectors share ONE egress client and host
	// spacer; AppEnv and EgressAllowHosts equal the client's.
	pcfg := poller.ConfigFrom(a, cfg.BillingEnabled)
	core := poller.Deps{Pool: pool, Cfg: pcfg, ExecutorFor: watchPollExecutor(cfg)}
	src := poller.SourceDeps{Deps: core, Client: wbDeps.Client, SecUserAgent: os.Getenv("SEC_EDGAR_USER_AGENT"),
		FeedMaxEntries: a.PollerFeedMaxEntries, Spacer: poller.NewHostSpacer(time.Second)}
	py := poller.PyDeps{SourceDeps: src, AppEnv: cfg.AppEnv, EgressAllowHosts: cfg.EgressAllowHosts,
		Py: poller.PyCollectors{JobSpyMaxJobs: a.PollerJobspyMaxJobs, JobChangeMaxContactsPerPoll: a.PollerJobChangeMaxContactsPerPoll}}
	if provRPC != nil { // in-process providers first, then the worker (rpclocal); nil keeps ErrUnavailable
		py.Py.Client = provRPC
	}
	pollDeps := poller.HandlerDeps{PyDeps: py, Hook: emitter, Enabled: cfg.Flags.IntentPollerEnabled,
		PGStore: cfg.Flags.PGLeadStore, Backfill: a.PollerBackfill, Log: log}
	queue.RegisterFailureHook(jobWatchPoll, func(ctx context.Context, n queue.FailureNotice) error {
		return poller.Reconcile(ctx, core, n)
	})

	crmDeps := crmpush.Deps{HubSpot: &crm.HubSpot{Secrets: local}, Salesforce: &crm.Salesforce{Secrets: local}, Pool: pool, Log: log}
	evalDeps := autoengine.Deps{Pool: pool, Log: log, Enabled: cfg.Flags.AutomationsEnabled, BillingEnabled: cfg.BillingEnabled,
		MaxRowsPerEval: a.MaxRowsPerEval, GlobalDailyUSD: a.GlobalDailyUSD,
		Actions: autoactions.Deps{Webhook: autoactions.NewWebhookDeps(cfg.AppEnv, cfg.EgressAllowHosts, a, sec, log),
			CRM: crmDeps, Push: crmPushGuard(local, a.GoExecutorTriggerEval, log), RunRows: autoactions.WorkbookRowRunner(wbDeps),
			LegacyOutreach: a.GoExecutorLegacyOutreachActions, Pool: pool,
			SendExecutor: func(ws string) string { return outreachsend.ExecutorFor(cfg.Executor, cfg.Flags.Outreach, ws) }},
		ExecFor: triggerEvalExecutor(cfg), Emitter: emitter, PyColumns: pyRPC != nil, LegacyOutreach: a.GoExecutorLegacyOutreachActions}

	handlers := map[string]runner{
		jobTriggerEval: handBackInert(pool, "GO_EXECUTOR_TRIGGER_EVAL is off"),
		jobWatchPoll:   handBackInert(pool, "GO_EXECUTOR_WATCH_POLL is off"),
	}
	if a.GoExecutorTriggerEval {
		handlers[jobTriggerEval] = func(ctx context.Context, c *queue.Claim) error { return autoengine.Handle(ctx, evalDeps, c) }
	}
	if a.GoExecutorWatchPoll {
		handlers[jobWatchPoll] = func(ctx context.Context, c *queue.Claim) error { return poller.Handle(ctx, pollDeps, c) }
	}
	return handlers
}
