package schedboot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoschedule"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadjobs"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/poller"
)

// Deps is everything the bootstraps read. The mirror tables they scan carry no RLS, so Pool needs no
// tenant; each enqueue re-enters the workspace (tenancy.InWorkspace).
type Deps struct {
	Pool *pgxpool.Pool
	// Cfg supplies the feature flags and the executor policy: the same config.Executor / flag functions
	// the API uses decide each job's executor column.
	Cfg config.Config
	// Now is the clock (default time.Now).
	Now func() time.Time
	Log *slog.Logger
	// IMAPConfigured is is_imap_configured(workspace): whether the workspace has IMAP credentials. nil
	// means the secret store is unavailable (CREDENTIALS_SOURCE=http) and the inbound bootstrap reports it.
	IMAPConfigured func(ctx context.Context, workspaceID string) (bool, error)
	// Getenv reads the source-health probe flags (default os.Getenv via leadjobs).
	Getenv func(string) string
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

func (d Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// Subsystems lists the nine bootstraps in the order of apps/api/scheduler.py.
func (d Deps) Subsystems() []Subsystem {
	wrap := func(name string, f func(context.Context) (any, error)) Subsystem {
		return Subsystem{Name: name, Run: f}
	}
	return []Subsystem{
		wrap("signal_scan", func(ctx context.Context) (any, error) { return d.SignalScan(ctx) }),
		wrap("automations", func(ctx context.Context) (any, error) { return d.Automations(ctx) }),
		wrap("outreach", func(ctx context.Context) (any, error) { return d.Outreach(ctx) }),
		wrap("outreach_inbound", func(ctx context.Context) (any, error) { return d.OutreachInbound(ctx) }),
		wrap("intent_poller", func(ctx context.Context) (any, error) { return d.IntentPoller(ctx) }),
		wrap("source_health", func(ctx context.Context) (any, error) { return d.SourceHealth(ctx) }),
		wrap("audiences", func(ctx context.Context) (any, error) { return d.Audiences(ctx) }),
		wrap("retention", func(ctx context.Context) (any, error) { return d.Retention(ctx) }),
		wrap("playbooks", func(ctx context.Context) (any, error) { return d.Playbooks(ctx) }),
	}
}

// triggerEvalExecutor is the cohort rule of trigger_eval and its scheduled occurrences.
func (d Deps) triggerEvalExecutor() func(string) string {
	return automationhook.CohortExecutor(d.Cfg.Executor.Enabled, d.Cfg.Flags.Automations.GoExecutorTriggerEval, d.Cfg.Executor.Workspaces)
}

func (d Deps) watchPollExecutor() func(string) string {
	return automationhook.CohortExecutor(d.Cfg.Executor.Enabled, d.Cfg.Flags.Automations.GoExecutorWatchPoll, d.Cfg.Executor.Workspaces)
}

func (d Deps) audienceExecutor() func(string) string {
	return automationhook.CohortExecutor(d.Cfg.Executor.Enabled, d.Cfg.Flags.Audiences.GoExecutorAudienceRefresh, d.Cfg.Executor.Workspaces)
}

// Automations is bootstrap_schedules (AUTOMATIONS_ENABLED): due on_schedule triggers.
func (d Deps) Automations(ctx context.Context) (int, error) {
	return autoschedule.BootstrapSchedulesIf(ctx, d.Pool, d.Cfg.Flags.AutomationsEnabled, d.now(), d.triggerEvalExecutor())
}

// IntentPoller is bootstrap_watch_schedules (INTENT_POLLER_ENABLED, PG lead store).
func (d Deps) IntentPoller(ctx context.Context) (int, error) {
	return poller.BootstrapWatchSchedulesIf(ctx, d.Pool, d.Cfg.Flags.IntentPollerEnabled, d.Cfg.Flags.PGLeadStore, d.now(),
		poller.ConfigFrom(d.Cfg.Flags.Automations, d.Cfg.BillingEnabled), d.watchPollExecutor())
}

// SourceHealth is bootstrap_source_health (SOURCE_HEALTH_ENABLED): one health job when none is pending.
func (d Deps) SourceHealth(ctx context.Context) (bool, error) {
	ld := leadjobs.Deps{Pool: d.Pool, Getenv: d.Getenv, Executor: d.Cfg.Executor.SourceHealthExecutor, Now: d.now, Log: d.log()}
	if ld.Getenv == nil {
		ld.Getenv = osGetenv
	}
	if !ld.ProbeEnabled() {
		return false, nil
	}
	var created bool
	err := inTx(ctx, d.Pool, func(tx pgx.Tx) (err error) {
		created, err = leadjobs.BootstrapSourceHealth(ctx, tx, ld)
		return err
	})
	return created, err
}

// inTx runs fn in one transaction on the pool (no tenant).
func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// perRow joins the errors of rows that failed; a failing row never stops the next one.
type perRow struct{ errs []error }

func (p *perRow) add(kind, id string, err error) {
	p.errs = append(p.errs, fmt.Errorf("%s %s: %w", kind, id, err))
}

func (p *perRow) err() error { return errors.Join(p.errs...) }
