package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/imapclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachcfg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachinbound"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachsend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachtoken"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/smtpclient"
)

// outreachInboundTimeout is the wall-clock limit of one outreach_inbound_poll job
// (Python JOB_TIMEOUTS has no entry: DEFAULT_JOB_TIMEOUT 600).
// jobSend is the send job type (Python JOB_TIMEOUTS["send"] = 300).
const jobSend = "send"

const outreachInboundTimeout = 600 * time.Second

// outreachOpts are test seams. The zero value is the production wiring: the SSRF-guarded
// SMTP/IMAP dial with no plaintext host and no custom roots, plan 08 autocaps.
type outreachOpts struct {
	mailer  outreachsend.Mailer
	newIMAP func(outreachcfg.IMAP) imapclient.Client
	noCaps  bool
}

// outreachMasterKey resolves the per-workspace secrets master key exactly like Python
// _load_master_key; the unsubscribe signing key derives from it. insecureDefault is
// strings.Contains(SECRET_KEY, INSECURE_DEFAULT_SECRET_KEY), like Python.
func outreachMasterKey(cfg config.Config) ([]byte, error) {
	return secrets.MasterKeyBytes(cfg.SecretsMasterKey, cfg.SecretKey,
		strings.Contains(cfg.SecretKey, config.InsecureDefaultSecretKey), config.IsDevEnv(cfg.AppEnv))
}

// outreachHandlers builds the send and inbound-poll handlers.
func outreachHandlers(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, local *secrets.Resolver,
	master []byte, o outreachOpts) (*outreachsend.Handler, *outreachinbound.Handler) {
	out := cfg.Flags.Outreach
	dial := egress.NewDialContext(cfg.AppEnv, cfg.EgressAllowHosts)
	var mailer outreachsend.Mailer = &outreachsend.SMTPMailer{Client: &smtpclient.Client{}, Dial: dial}
	if o.mailer != nil {
		mailer = o.mailer
	}
	var caps outreachsend.Caps
	if !o.noCaps {
		caps = outreachsend.AutoCaps{GlobalDailyUSD: cfg.Flags.Automations.GlobalDailyUSD}
	}
	send := &outreachsend.Handler{
		Pool: pool, Secrets: local, Signer: outreachtoken.NewSigner(master, out.PublicBaseURL, out.UnsubTTLDays, nil),
		Mailer: mailer, Caps: caps, Automations: cfg.Flags.AutomationsEnabled, BillingEnabled: cfg.BillingEnabled,
		SendCostUSD: out.SendCostUSD, Thresholds: outreachstore.ThresholdsFrom(out), Log: log,
	}
	inbound := &outreachinbound.Handler{
		Pool: pool, Secrets: local, Automations: cfg.Flags.AutomationsEnabled, Outreach: out, Executor: cfg.Executor,
		Dial: dial, NewClient: o.newIMAP, Log: log,
	}
	return send, inbound
}

// buildOutreach registers `send` and `outreach_inbound_poll` as claimable and returns their
// handlers. Python decides the executor at enqueue (cohort + per-type flag), so Go claims
// by type on executor='go' rows only. A row that reaches this worker while its per-type
// flag is off, or while outreach cannot run here (no secret resolver under
// CREDENTIALS_SOURCE=http, or no usable master key: D6), is handed back to Python untouched.
func buildOutreach(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, local *secrets.Resolver, o outreachOpts) map[string]runner {
	queue.AllowJobType(jobSend)
	queue.AllowJobType(outreachinbound.JobType)
	out := cfg.Flags.Outreach
	handlers := map[string]runner{
		jobSend:                 handBackInert(pool, "GO_EXECUTOR_OUTREACH_SEND is off"),
		outreachinbound.JobType: handBackInert(pool, "GO_EXECUTOR_OUTREACH_INBOUND is off"),
	}
	if !out.GoExecutorSend && !out.GoExecutorInbound {
		return handlers
	}
	if local == nil {
		log.Warn("outreach handlers hand jobs back: no secret resolver; set CREDENTIALS_SOURCE=local")
		return inertOutreach(handlers, pool, "outreach needs CREDENTIALS_SOURCE=local")
	}
	master, err := outreachMasterKey(cfg)
	if err != nil {
		log.Error("outreach handlers hand jobs back: no usable master key", "err", err.Error())
		return inertOutreach(handlers, pool, "outreach master key unavailable")
	}
	send, inbound := outreachHandlers(cfg, pool, log, local, master, o)
	if out.GoExecutorSend {
		handlers[jobSend] = func(ctx context.Context, c *queue.Claim) error { return send.Handle(ctx, c) }
	}
	if out.GoExecutorInbound {
		handlers[outreachinbound.JobType] = func(ctx context.Context, c *queue.Claim) error { return inbound.Handle(ctx, c) }
	}
	return handlers
}

func inertOutreach(handlers map[string]runner, pool *pgxpool.Pool, why string) map[string]runner {
	for typ := range handlers {
		handlers[typ] = handBackInert(pool, why)
	}
	return handlers
}
