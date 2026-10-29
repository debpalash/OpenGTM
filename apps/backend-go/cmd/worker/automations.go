package main

import (
	"context"
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
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/poller"
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

// automationEmitter is the one on_rows_changed/on_signal/audience emitter the worker shares
// (workbook run-end flush, poller signals, ambitionbox, signal scan). Its jobs are executor go.
func automationEmitter(cfg config.Config, log *slog.Logger) automationhook.Emitter {
	return automationhook.NewWithOptions(cfg.Flags.AutomationsEnabled, nil, log, cfg.Flags.Automations.PollerMaxFiresPerSignal)
}

// logAutomationFlags records the effective plan 08 flags once at startup. The worker cannot
// verify that they equal the API's (AUTOMATIONS_ENABLED must match on both stacks).
func logAutomationFlags(log *slog.Logger, cfg config.Config) {
	log.Info("automation flags",
		"automations_enabled", cfg.Flags.AutomationsEnabled,
		"intent_poller_enabled", cfg.Flags.IntentPollerEnabled,
		"pg_lead_store", cfg.Flags.PGLeadStore,
		"allow_legacy_outreach", cfg.Flags.AutomationsAllowLegacyOutreach,
		"billing_enabled", cfg.BillingEnabled)
	// safecond reads naive timestamps in the process-local zone: the api and the worker must run
	// with the same TZ.
	log.Info("process timezone", "tz", time.Local.String())
}

// buildAutomations registers trigger_eval and watch_poll (claimable, failure hook for watch_poll) and returns
// their handlers. The heartbeat and cancel watcher of processJob covers both handlers, including a single-source
// watch_poll or a long re_enrich that only check the lease between steps. wbDeps feeds re_enrich; local resolves
// webhook header secrets and CRM tokens; emitter is the shared emitter; provRPC runs the provider collectors.
func buildAutomations(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, wbDeps workbook.Deps,
	local *secrets.Resolver, provRPC poller.PyCaller, emitter automationhook.Emitter) map[string]runner {
	a := cfg.Flags.Automations
	queue.AllowJobType(jobTriggerEval)
	queue.AllowJobType(jobWatchPoll)

	// The poller core, HTTP sources and provider collectors share ONE egress client and host
	// spacer; AppEnv and EgressAllowHosts equal the client's.
	pcfg := poller.ConfigFrom(a, cfg.BillingEnabled)
	core := poller.Deps{Pool: pool, Cfg: pcfg}
	src := poller.SourceDeps{Deps: core, Client: wbDeps.Client, SecUserAgent: os.Getenv("SEC_EDGAR_USER_AGENT"),
		FeedMaxEntries: a.PollerFeedMaxEntries, Spacer: poller.NewHostSpacer(time.Second)}
	py := poller.PyDeps{SourceDeps: src, AppEnv: cfg.AppEnv, EgressAllowHosts: cfg.EgressAllowHosts,
		Py: poller.PyCollectors{JobSpyMaxJobs: a.PollerJobspyMaxJobs, JobChangeMaxContactsPerPoll: a.PollerJobChangeMaxContactsPerPoll}}
	if provRPC != nil { // the in-process provider caller; nil keeps ErrUnavailable
		py.Py.Client = provRPC
	}
	pollDeps := poller.HandlerDeps{PyDeps: py, Hook: emitter, Enabled: cfg.Flags.IntentPollerEnabled,
		PGStore: cfg.Flags.PGLeadStore, Backfill: a.PollerBackfill, Log: log}
	queue.RegisterFailureHook(jobWatchPoll, func(ctx context.Context, n queue.FailureNotice) error {
		return poller.Reconcile(ctx, core, n)
	})

	var sec autoactions.SecretGetter
	if local != nil { // never a typed-nil resolver behind the interface
		sec = local
	}
	crmDeps := crmpush.Deps{HubSpot: &crm.HubSpot{Secrets: local}, Salesforce: &crm.Salesforce{Secrets: local}, Pool: pool, Log: log}
	evalDeps := autoengine.Deps{Pool: pool, Log: log, Enabled: cfg.Flags.AutomationsEnabled, BillingEnabled: cfg.BillingEnabled,
		MaxRowsPerEval: a.MaxRowsPerEval, GlobalDailyUSD: a.GlobalDailyUSD,
		Actions: autoactions.Deps{Webhook: autoactions.NewWebhookDeps(cfg.AppEnv, cfg.EgressAllowHosts, a, sec, log),
			CRM: crmDeps, RunRows: autoactions.WorkbookRowRunner(wbDeps), Pool: pool},
		Emitter: emitter}

	return map[string]runner{
		jobTriggerEval: func(ctx context.Context, c *queue.Claim) error { return autoengine.Handle(ctx, evalDeps, c) },
		jobWatchPoll:   func(ctx context.Context, c *queue.Claim) error { return poller.Handle(ctx, pollDeps, c) },
	}
}
