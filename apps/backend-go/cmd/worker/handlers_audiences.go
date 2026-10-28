package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
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
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
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
	http  *http.Client                                      // replaces the egress client of the CRM/campaign/sheet/Airtable ports
}

// audienceHandlers returns the plan 10 job handlers keyed by job type: audience_refresh and
// audience_destination_sync, registered unconditionally since the plan 15 cutover (GO_EXECUTOR_AUDIENCE_REFRESH and
// GO_DEST_TYPES are retired). audience_refresh fails permanently without an audience emitter (membership events
// would be dropped); every destination type the API accepts has a Go delivery port. Failure hooks mirror
// the Python parent-worker callbacks.
func audienceHandlers(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, sec destdeliver.SecretResolver,
	emitter audiencerefresh.Emitter, o audienceOpts) map[string]runner {
	queue.AllowJobType(audiencesched.JobType)
	queue.AllowJobType(deststore.JobType)
	handlers := map[string]runner{deststore.JobType: destSyncHandler(cfg, pool, log, sec, o)}
	if emitter == nil {
		log.Warn("audience_refresh jobs will fail: no audience event emitter wired")
		handlers[audiencesched.JobType] = refuseJob(audiencesched.JobType, "no audience event emitter")
		return handlers
	}
	queue.RegisterFailureHook(audiencesched.JobType, refreshFailureHook(pool, nil, log))
	handlers[audiencesched.JobType] = refreshHandler(pool, log, emitter, cfg.Flags.Audiences.RefreshPageSize, o.leads)
	return handlers
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

var (
	errPayloadWorkspace = errors.New("job payload workspace does not match the job row")
	errNoJobWorkspace   = errors.New("the job has no workspace_id; the tenant comes from the job row only")
)

// refreshFailureHook is reconcile_audience_refresh_failure after a persisted failed attempt. The tenant is the
// claimed job row's workspace only: a job without one, or whose payload names another, is logged and left alone
// (the payload workspace is never used as the tenant).
func refreshFailureHook(pool *pgxpool.Pool, execFor func(ws string) string, log *slog.Logger) queue.FailureHook {
	return func(ctx context.Context, n queue.FailureNotice) error {
		p := idPayload(n.Payload)
		if p == nil {
			return errors.New("audience refresh failure payload requires workspace_id and audience_id")
		}
		if n.WorkspaceID == "" {
			log.Warn("audience_refresh failure not reconciled: the job has no workspace_id", "job", n.JobID)
			return nil
		}
		if payloadStr(p, "workspace_id") != n.WorkspaceID {
			log.Warn("audience_refresh failure not reconciled: payload workspace is not the job's", "job", n.JobID)
			return nil
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
// removes its schedule. The tenant is the job row's workspace only (plan 15 cutover): a job without one, or whose
// payload names another, is refused permanently.
func refreshHandler(pool *pgxpool.Pool, log *slog.Logger, emitter audiencerefresh.Emitter, pageSize int,
	leads audiencerefresh.LeadSource) runner {
	return func(ctx context.Context, c *queue.Claim) error {
		p := idPayload(c.Payload)
		ws, id := payloadStr(p, "workspace_id"), payloadStr(p, "audience_id")
		if c.WorkspaceID == "" {
			return &queue.PermanentError{Err: errNoJobWorkspace}
		}
		if ws == "" || id == "" {
			log.Warn("audience_refresh missing workspace_id/audience_id", "job", c.ID)
			return nil
		}
		if c.WorkspaceID != ws {
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
			AudienceEvents: emitter, PageSize: pageSize, Leads: leads,
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
			if serr := scheduleAfter(sctx, pool, ws, id); serr != nil {
				log.Error("audience_refresh: schedule next after failure", "audience", id, "err", serr)
			}
			return rerr
		}
		return scheduleAfter(ctx, pool, ws, id)
	}
}

// scheduleAfter is schedule_next for the audience as it now stands.
func scheduleAfter(ctx context.Context, pool *pgxpool.Pool, ws, id string) error {
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
		}, time.Now(), "go")
		return err
	})
}

// destSyncHandler wires the destination sync engine. Every type the API accepts has a Go delivery port; a run whose
// type has none (ports.Supports: an unknown stored type) fails permanently and the failure hook reconciles the run.
func destSyncHandler(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, sec destdeliver.SecretResolver, o audienceOpts) runner {
	aud := cfg.Flags.Audiences
	ports := destPorts(cfg, pool, log, sec, o)
	get := adTokenGetter(sec, adTokensWorkspaceOnly())
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
	return func(ctx context.Context, c *queue.Claim) error {
		p := idPayload(c.Payload)
		ws, run := payloadStr(p, "workspace_id"), payloadStr(p, "run_id")
		if c.WorkspaceID == "" {
			return &queue.PermanentError{Err: errNoJobWorkspace}
		}
		if ws == "" || run == "" || c.WorkspaceID != ws {
			return eng.Handle(ctx, c) // the engine refuses a bad payload permanently
		}
		destType, found, err := destinationType(ctx, pool, ws, run)
		if err != nil {
			return err
		}
		if found && !ports.Supports(destType) {
			return &queue.PermanentError{Err: fmt.Errorf("destination type %s has no Go delivery port", destType)}
		}
		return eng.Handle(ctx, c)
	}
}

// destPorts builds the delivery ports; a port set in o.ports wins. Webhook and Slack deliver through the
// egress-pinned automation sender with the outbound secret-ref policy (S-M3, R2); the same policy gates the warehouse
// and Slack references at delivery. HubSpot, Salesforce, Instantly, Smartlead, Google Sheets and Airtable deliver
// through destdeliver.NewLivePorts over the egress client with the workspace's own credentials (sec must be the
// local resolver; any other resolver resolves nothing for them, so they fail "not connected").
func destPorts(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, sec destdeliver.SecretResolver, o audienceOpts) destdeliver.Ports {
	ports := o.ports
	if ports.Secrets == nil && sec != nil {
		ports.Secrets = sec
	}
	wd := autoactions.NewWebhookDeps(cfg.AppEnv, cfg.EgressAllowHosts, cfg.Flags.Automations, sec, log)
	if ports.Webhook == nil {
		ports.Webhook = destdeliver.AutoWebhook{Deps: wd}
	}
	if ports.Outbound.Prefixes == nil && ports.Outbound.Registered == nil {
		ports.Outbound = wd.Outbound
	}
	hc := o.http
	if hc == nil {
		hc = egress.NewClientForEnv(cfg.AppEnv, cfg.EgressAllowHosts)
	}
	local, _ := sec.(*secrets.Resolver)
	lp := destdeliver.NewLivePorts(destdeliver.LiveConfig{Pool: pool, Secrets: local, HTTP: hc, Log: log})
	if ports.CRM == nil {
		ports.CRM = lp.CRM
	}
	if ports.Instantly == nil {
		ports.Instantly = lp.Instantly
	}
	if ports.Smartlead == nil {
		ports.Smartlead = lp.Smartlead
	}
	if ports.Sheets == nil {
		ports.Sheets = lp.Sheets
	}
	if ports.Airtable == nil {
		ports.Airtable = lp.Airtable
	}
	return ports
}

// adTokensWorkspaceOnly is AD_TOKENS_WORKSPACE_ONLY (default off): with it on, ad-platform tokens resolve from
// workspace secrets only, like the other destination references (S-L25; the default keeps the global and env
// fallback the plan 10 ruling accepted).
func adTokensWorkspaceOnly() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AD_TOKENS_WORKSPACE_ONLY"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// adTokenGetter resolves an ad-platform token: workspace, then global and env, or workspace only.
func adTokenGetter(sec destdeliver.SecretResolver, workspaceOnly bool) adsdrv.SecretGetter {
	if workspaceOnly {
		return destdeliver.WorkspaceOnly(sec)
	}
	return func(ctx context.Context, ws, name string) (string, error) {
		if sec == nil {
			return "", nil
		}
		v, _, err := sec.Get(ctx, ws, name)
		return v, err
	}
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
