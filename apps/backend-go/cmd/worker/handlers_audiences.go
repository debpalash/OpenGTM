package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/adsdrv"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencerefresh"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencesched"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoactions"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destdeliver"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/deststore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destsync"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/warehouse"
)

// audienceLimit is the wall-clock limit of an audience job: Python JOB_TIMEOUTS
// (audience_refresh 900, audience_destination_sync 1800).
func audienceLimit(jobType string) (time.Duration, bool) {
	switch jobType {
	case audiencesched.JobType:
		return audiencerefresh.JobTimeout, true
	case deststore.JobType:
		return 1800 * time.Second, true
	}
	return 0, false
}

// audienceOpts are test seams; the zero value is the production wiring.
type audienceOpts struct {
	ports destdeliver.Ports                                 // replaces the delivery ports built from the worker's secrets
	leads audiencerefresh.LeadSource                        // replaces the lead query of a refresh
	lease func(context.Context, *queue.Claim) (bool, error) // replaces the sync engine's lease check
}

// audienceHandlers returns the plan 10 job handlers keyed by job type: audience_refresh
// (GO_EXECUTOR_AUDIENCE_REFRESH) and audience_destination_sync (GO_DEST_TYPES). Python decides the
// executor at enqueue (cohort + flag), so Go claims by type on executor='go' rows only, and both
// types are always claimable: a row whose handler is off is handed back to Python, never failed as
// unsupported. audience_refresh is handed back while its flag is off or no audience emitter is
// wired (membership events would be dropped). audience_destination_sync hands back every row whose
// destination type is not in GO_DEST_TYPES or has no Go delivery port (destsync.Eligible). Failure
// hooks mirror the Python parent-worker callbacks and run only for the real handlers.
func audienceHandlers(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, sec destdeliver.SecretResolver,
	emitter audiencerefresh.Emitter, o audienceOpts) map[string]runner {
	queue.AllowJobType(audiencesched.JobType)
	queue.AllowJobType(deststore.JobType)
	handlers := map[string]runner{
		audiencesched.JobType: handBackInert(pool, "GO_EXECUTOR_AUDIENCE_REFRESH is off"),
	}
	aud := cfg.Flags.Audiences
	switch {
	case !aud.GoExecutorAudienceRefresh:
	case emitter == nil:
		log.Warn("audience_refresh jobs are handed back to Python: no audience event emitter wired")
		handlers[audiencesched.JobType] = handBackInert(pool, "audience emitter unavailable")
	default:
		execFor := func(ws string) string { return cohortExecutor(cfg, aud.GoExecutorAudienceRefresh, ws) }
		queue.RegisterFailureHook(audiencesched.JobType, refreshFailureHook(pool, execFor))
		h := refreshHandler(pool, log, emitter, execFor, destExecutorFor(cfg), aud.RefreshPageSize, o.leads)
		handlers[audiencesched.JobType] = h
	}
	handlers[deststore.JobType] = destSyncHandler(cfg, pool, log, sec, o)
	return handlers
}

// cohortExecutor is "go" only with GO_EXECUTOR_ENABLED, the per-type flag and the workspace in the cohort.
func cohortExecutor(cfg config.Config, flag bool, ws string) string {
	if cfg.Executor.Enabled && flag && cfg.Executor.Workspaces[ws] {
		return "go"
	}
	return "python"
}

// destExecutorFor is go_executor.destination_executor: "go" only with GO_EXECUTOR_ENABLED, the
// workspace in the cohort and the destination type in GO_DEST_TYPES.
func destExecutorFor(cfg config.Config) func(ws string) func(destType string) string {
	types := goDestTypes(cfg)
	return func(ws string) func(string) string {
		return func(destType string) string {
			if cfg.Executor.Enabled && cfg.Executor.Workspaces[ws] && types[strings.ToLower(strings.TrimSpace(destType))] {
				return "go"
			}
			return "python"
		}
	}
}

func goDestTypes(cfg config.Config) map[string]bool {
	m := map[string]bool{}
	for _, t := range cfg.Flags.Audiences.GoDestTypes {
		m[strings.ToLower(strings.TrimSpace(t))] = true
	}
	return m
}

// idPayload decodes a job payload object (nil when it is not an object).
func idPayload(raw []byte) map[string]any {
	var m map[string]any
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&m); err != nil {
		return nil
	}
	return m
}

func payloadStr(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

var errPayloadWorkspace = errors.New("job payload workspace does not match the job row")

// refreshFailureHook is reconcile_audience_refresh_failure after a persisted failed attempt. The
// payload workspace must be the job row's own; the work then runs in that tenant.
func refreshFailureHook(pool *pgxpool.Pool, execFor func(ws string) string) queue.FailureHook {
	return func(ctx context.Context, n queue.FailureNotice) error {
		p := idPayload(n.Payload)
		if p == nil {
			return errors.New("audience refresh failure payload requires workspace_id and audience_id")
		}
		if ws := payloadStr(p, "workspace_id"); n.WorkspaceID != "" && ws != n.WorkspaceID {
			return errPayloadWorkspace
		}
		return audiencesched.ReconcileFailure(ctx, pool, execFor, n.JobID, p, n.Reason, n.WillRetry, time.Now())
	}
}

// destFailureHook is reconcile_destination_job_failure (Engine.OnFailure checks the workspace).
func destFailureHook(eng *destsync.Engine, pool *pgxpool.Pool) queue.FailureHook {
	return func(ctx context.Context, n queue.FailureNotice) error {
		return eng.OnFailure(ctx, pool, &queue.Claim{ID: n.JobID, Type: n.JobType, WorkspaceID: n.WorkspaceID, Payload: n.Payload}, n.Reason, n.WillRetry)
	}
}

// refreshHandler is handle_audience_refresh: RefreshObserved, then ScheduleNext; on error the next
// occurrence is scheduled first and the error returned. A missing or refresh-disabled audience
// removes its schedule.
func refreshHandler(pool *pgxpool.Pool, log *slog.Logger, emitter audiencerefresh.Emitter, execFor func(string) string,
	destExec func(ws string) func(string) string, pageSize int, leads audiencerefresh.LeadSource) runner {
	return func(ctx context.Context, c *queue.Claim) error {
		p := idPayload(c.Payload)
		ws, id := payloadStr(p, "workspace_id"), payloadStr(p, "audience_id")
		if ws == "" || id == "" {
			log.Warn("audience_refresh missing workspace_id/audience_id", "job", c.ID)
			return nil
		}
		if c.WorkspaceID != "" && c.WorkspaceID != ws {
			return &queue.PermanentError{Err: errPayloadWorkspace}
		}
		removed := false
		err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
			row, err := gen.New(tx).AudSchedLockAudience(ctx, gen.AudSchedLockAudienceParams{ID: id, WorkspaceID: ws})
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && !row.RefreshEnabled) {
				removed = true
				return audiencesched.RemoveSchedule(ctx, tx, id)
			}
			return err
		})
		if err != nil || removed {
			return err
		}
		_, rerr := audiencerefresh.RefreshObserved(ctx, pool, ws, id, audiencerefresh.Deps{
			AudienceEvents: emitter, ExecutorFor: destExec(ws), PageSize: pageSize, Leads: leads,
			Timeout: audiencerefresh.JobTimeout,
		})
		if errors.Is(rerr, audiencerefresh.ErrNotFound) || errors.Is(rerr, tenancy.ErrNoWorkspace) {
			return nil // deleted since the check above
		}
		if rerr != nil {
			// A source failure must not strand a recurring audience: persist the next
			// occurrence (even when ctx is done) before the queue retries or dead-letters.
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if serr := scheduleAfter(sctx, pool, ws, id, execFor(ws)); serr != nil {
				log.Error("audience_refresh: schedule next after failure", "audience", id, "err", serr)
			}
			return rerr
		}
		return scheduleAfter(ctx, pool, ws, id, execFor(ws))
	}
}

// scheduleAfter is schedule_next for the audience as it now stands.
func scheduleAfter(ctx context.Context, pool *pgxpool.Pool, ws, id, executor string) error {
	return tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		row, err := gen.New(tx).AudSchedLockAudience(ctx, gen.AudSchedLockAudienceParams{ID: id, WorkspaceID: ws})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = audiencesched.ScheduleNext(ctx, tx, audiencesched.ScheduleInput{
			ID: id, WorkspaceID: ws, RefreshEnabled: row.RefreshEnabled, IntervalMinutes: int(row.RefreshIntervalMinutes),
		}, time.Now(), executor)
		return err
	})
}

// destSyncHandler wires the destination sync engine. Every row whose destination type is not
// eligible (destsync.Eligible: in GO_DEST_TYPES and deliverable with the wired ports) is handed
// back to Python untouched.
func destSyncHandler(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, sec destdeliver.SecretResolver, o audienceOpts) runner {
	aud := cfg.Flags.Audiences
	ports := o.ports
	if ports.Secrets == nil && sec != nil {
		ports.Secrets = sec
	}
	// Webhook and Slack deliver through the egress-pinned automation sender with the outbound secret-ref
	// policy (S-M3, R2); the same policy gates the warehouse and Slack references at delivery.
	wd := autoactions.NewWebhookDeps(cfg.AppEnv, cfg.EgressAllowHosts, cfg.Flags.Automations, sec, log)
	if ports.Webhook == nil {
		ports.Webhook = destdeliver.AutoWebhook{Deps: wd}
	}
	if ports.Outbound.Prefixes == nil && ports.Outbound.Registered == nil {
		ports.Outbound = wd.Outbound
	}
	types := goDestTypes(cfg)
	for t := range types {
		if !provenanceEligible(sec, t) {
			log.Warn("GO_DEST_TYPES lists a type that needs CREDENTIALS_SOURCE=local; its jobs stay on Python", "type", t)
		}
		if !ports.Supports(t) {
			log.Warn("GO_DEST_TYPES lists a destination type without a Go delivery port; its jobs stay on Python", "type", t)
		}
	}
	get := func(ctx context.Context, ws, name string) (string, error) {
		if sec == nil {
			return "", nil
		}
		v, _, err := sec.Get(ctx, ws, name)
		return v, err
	}
	dd := destsync.Deps{
		Pool: pool, Deliverer: destdeliver.New(ports, aud.DestDeliveryTimeout),
		Ads:            destsync.NewAdSyncer(adsdrv.NewClient(get, cfg.AppEnv, cfg.EgressAllowHosts)),
		Warehouse:      destsync.NewWarehouseSyncer(warehouse.NewClient(cfg.AppEnv, cfg.EgressAllowHosts).WithOutbound(ports.Outbound), destdeliver.WorkspaceOnly(sec)),
		MemberPageSize: aud.DestMemberPageSize, AdBatchSize: aud.DestAdBatchSize, Log: log,
	}
	if o.lease != nil {
		dd.Lease, dd.LeaseEvery = o.lease, -1
	}
	eng := destsync.New(dd)
	queue.RegisterFailureHook(deststore.JobType, destFailureHook(eng, pool))
	hand := func(ctx context.Context, c *queue.Claim, why string) error { return handBackInert(pool, why)(ctx, c) }
	return func(ctx context.Context, c *queue.Claim) error {
		p := idPayload(c.Payload)
		ws, run := payloadStr(p, "workspace_id"), payloadStr(p, "run_id")
		if ws == "" || run == "" || (c.WorkspaceID != "" && c.WorkspaceID != ws) {
			return eng.Handle(ctx, c) // the engine refuses a bad payload permanently
		}
		destType, found, err := destinationType(ctx, pool, ws, run)
		if err != nil {
			return err
		}
		if found && !destsync.Eligible(destType, ports, types) {
			return hand(ctx, c, "destination type "+destType+" is not served by the Go worker")
		}
		if found && !provenanceEligible(sec, destType) {
			return hand(ctx, c, "destination type "+destType+" needs CREDENTIALS_SOURCE=local on the Go worker")
		}
		return eng.Handle(ctx, c)
	}
}

// provenanceEligible is false for the types whose secret refs must resolve workspace-only
// (warehouse_http, slack) when the worker's resolver is the credentials API (CREDENTIALS_SOURCE=http):
// it cannot report a value's source, so every delivery would fail. Those rows stay on Python.
func provenanceEligible(sec destdeliver.SecretResolver, destType string) bool {
	if _, api := sec.(apiSecrets); !api {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(destType)) {
	case "warehouse_http", "slack":
		return false
	}
	return true
}

// destinationType reads the type of the run's destination inside the run's tenant; a missing run
// or destination is left to the engine (found false).
func destinationType(ctx context.Context, pool *pgxpool.Pool, ws, runID string) (typ string, found bool, err error) {
	err = tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		run, err := deststore.GetRun(ctx, tx, runID)
		if errors.Is(err, deststore.ErrRunNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		d, err := deststore.GetDestination(ctx, tx, run.DestinationID)
		if errors.Is(err, deststore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		typ, found = strings.ToLower(strings.TrimSpace(d.Type)), true
		return nil
	})
	return
}
