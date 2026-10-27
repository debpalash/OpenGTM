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

// buildOutreach registers `send` and `outreach_inbound_poll` as claimable and returns their handlers, unconditionally
// since the plan 15 cutover (GO_EXECUTOR_OUTREACH_SEND / _INBOUND are retired). Without a secret resolver or a
// usable master key (D6) both job types fail permanently with the reason.
func buildOutreach(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, local *secrets.Resolver, o outreachOpts) map[string]runner {
	queue.AllowJobType(jobSend)
	queue.AllowJobType(outreachinbound.JobType)
	refuse := func(why string) map[string]runner {
		return map[string]runner{jobSend: refuseJob(jobSend, why), outreachinbound.JobType: refuseJob(outreachinbound.JobType, why)}
	}
	if local == nil {
		log.Warn("outreach jobs will fail: no secret resolver")
		return refuse("no secret resolver")
	}
	master, err := outreachMasterKey(cfg)
	if err != nil {
		log.Error("outreach jobs will fail: no usable master key", "err", err.Error())
		return refuse("outreach master key unavailable")
	}
	send, inbound := outreachHandlers(cfg, pool, log, local, master, o)
	return map[string]runner{
		jobSend:                 func(ctx context.Context, c *queue.Claim) error { return send.Handle(ctx, c) },
		outreachinbound.JobType: func(ctx context.Context, c *queue.Claim) error { return inbound.Handle(ctx, c) },
	}
}
