package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collect"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadprogress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

// collectOpts are the collect / bulk_enrich collaborators; a nil deps fails both job types.
type collectOpts struct {
	deps *collect.Deps // the shared pipeline (liveCollectDeps); nil: the jobs fail permanently
	why  string        // why deps is nil
}

// collectHandlers returns the plan 15 WP-COLLECT handlers collect and bulk_enrich, registered unconditionally since
// the cutover (GO_EXECUTOR_COLLECT and GO_EXECUTOR_BULK_ENRICH are retired). Without the pipeline (no browser
// service) both fail permanently with the reason.
func collectHandlers(pool *pgxpool.Pool, log *slog.Logger, o collectOpts) map[string]runner {
	queue.AllowJobType(collect.JobTypeCollect)
	queue.AllowJobType(collect.JobTypeBulkEnrich)
	collect.RegisterFailureHook(pool)
	if o.deps == nil {
		why := o.why
		if why == "" {
			why = "the collect pipeline is not wired"
		}
		log.Warn("collect and bulk_enrich jobs will fail", "reason", why)
		return map[string]runner{
			collect.JobTypeCollect:    refuseJob(collect.JobTypeCollect, why),
			collect.JobTypeBulkEnrich: refuseJob(collect.JobTypeBulkEnrich, why),
		}
	}
	d := *o.deps
	return map[string]runner{
		collect.JobTypeCollect:    func(ctx context.Context, c *queue.Claim) error { return collect.HandleCollect(ctx, d, c) },
		collect.JobTypeBulkEnrich: func(ctx context.Context, c *queue.Claim) error { return collect.HandleBulkEnrich(ctx, d, c) },
	}
}

// errNoBrowser: the pipeline needs the browser service for search and pages.
var errNoBrowser = errors.New("BROWSER_SERVICE_URL / BROWSER_SERVICE_TOKEN not set")

// liveCollectDeps wires the Go pipeline once per process: leadweb over the browser service, the scrapers, the
// global source switches and health gate, the email steps of internal/emailverify, providers through the
// in-process provider caller, an LLM client per workspace accounting usage to it, and the leadgen progress bus.
// The leadweb is returned for the source_health probes.
func liveCollectDeps(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, get func(string) string,
	prov collect.ProviderCaller, sh *workerShared) (*collect.Deps, *leadweb.Web, error) {
	cs, err := config.LoadCollect(get)
	if err != nil {
		return nil, nil, err
	}
	sc := config.ScrapeFrom(get)
	if sc.BrowserURL == "" || sc.BrowserToken == "" {
		return nil, nil, errNoBrowser
	}
	sw, err := workerLeadweb(cfg, get) // the worker's one leadweb (shared rate limits and cache)
	if err != nil {
		return nil, nil, err
	}
	bc, web := sw.Browser, sw.Web
	app, reg, llmCfg, local := sh.app, sh.reg, sh.llmCfg, sh.local
	httpc := egress.NewOperatorClient(cfg.EgressAllowHosts)
	var py emailverify.ProviderCaller
	if prov != nil {
		py = prov
	}
	mail := emailverify.NewLive(emailverify.LiveConfig{Pool: pool, AppEnv: cfg.AppEnv, EgressAllow: cfg.EgressAllowHosts,
		HTTP: httpc, Py: py, SMTPDisabled: get("LEADGEN_SMTP_VERIFY") == "0", HeloHost: get("LEADGEN_SMTP_HELO"),
		MailFrom: get("LEADGEN_SMTP_MAIL_FROM"),
		Keys: func(ctx context.Context, ws, envVar string) (string, error) {
			v, _, err := local.Get(ctx, ws, envVar)
			return v, err
		}})
	var bus *leadprogress.Bus
	if cfg.RedisURL != "" {
		opt, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return nil, nil, err
		}
		bus = leadprogress.NewBus(redis.NewClient(opt))
	} else {
		bus = leadprogress.NewBus(nil)
	}
	knobs := collect.DefaultKnobs()
	knobs.RegistrySourceCap, knobs.DMBudget = cs.RegistrySourceCap, cs.DMBudget
	knobs.EmailWaterfallBudget, knobs.DeliverabilityBudget = cs.EmailWaterfallBudget, cs.DeliverabilityBudget
	knobs.SourceReliability, knobs.SearchTimeout = cs.SourceReliability, cs.SearchTimeout
	sources := collect.LiveSources{Settings: app, Pool: pool, Enforce: cs.HealthEnforce}
	enrich := collect.LiveEnrich{Mail: mail, Caller: prov}
	d := &collect.Deps{
		Pool: pool,
		For: func(ws string) *collect.Runner {
			return &collect.Runner{Web: collect.LiveWeb{W: web, WS: ws}, Discovery: collect.LiveDiscovery{B: bc},
				Sources: sources, Enrich: enrich, Knobs: knobs,
				NewLLM: func() collect.LLM {
					return llm.New(llm.Options{Settings: app, HTTP: httpc, Usage: &llm.PgUsage{Pool: pool}, WorkspaceID: ws,
						Config: llmCfg, Registry: reg, Log: log})
				}}
		},
		Emit: func(ws string) collect.Emitter { return collect.BusEmitter{Bus: bus, WS: ws} },
	}
	log.Info("collect pipeline wired", "provider_caller", prov != nil)
	return d, web, nil
}
