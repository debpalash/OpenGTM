package main

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collect"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/credentials"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadjobs"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadprogress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
)

// collectOpts are test seams; the zero value is the production wiring.
type collectOpts struct {
	getenv func(string) string  // nil: config.Getenv (GO_ALL_NATIVE applies)
	deps   *collect.Deps        // replaces the live runner wiring
	web    collect.Web          // the native prober's web (nil: leadweb over the browser service)
	health func() leadjobs.Deps // nil: the production source_health deps
}

// registerCollect adds the plan 15 WP-COLLECT handlers to handlers (the one-line wiring in main):
// collect and bulk_enrich (GO_EXECUTOR_COLLECT / GO_EXECUTOR_BULK_ENRICH; rows whose flag is off,
// or that this worker cannot run without a browser service, are handed back to Python), and with
// GO_NATIVE_SOURCE_PROBE a source_health_check that probes in process instead of through the
// Python worker. Call it after leadgenHandlers.
func registerCollect(handlers map[string]runner, cfg config.Config, pool *pgxpool.Pool, log *slog.Logger,
	prov collect.ProviderCaller, local *secrets.Resolver, api *credentials.Client) {
	for typ, h := range collectHandlers(cfg, pool, log, prov, local, api, collectOpts{}) {
		handlers[typ] = h
	}
}

func collectHandlers(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, prov collect.ProviderCaller,
	local *secrets.Resolver, api *credentials.Client, o collectOpts) map[string]runner {
	queue.AllowJobType(collect.JobTypeCollect)
	queue.AllowJobType(collect.JobTypeBulkEnrich)
	get := o.getenv
	if get == nil {
		get = config.Getenv
	}
	out := map[string]runner{
		collect.JobTypeCollect:    handBackInert(pool, "GO_EXECUTOR_COLLECT is off"),
		collect.JobTypeBulkEnrich: handBackInert(pool, "GO_EXECUTOR_BULK_ENRICH is off"),
	}
	cs, err := config.LoadCollect(get)
	if err != nil {
		log.Error("collect jobs are handed back to Python: invalid setting", "err", err)
		return out
	}
	collect.RegisterFailureHook(pool)
	var d *collect.Deps
	var web *leadweb.Web
	if o.deps != nil {
		d = o.deps
	} else if cs.GoCollect || cs.GoBulkEnrich || cs.NativeProbe {
		d, web, err = liveCollectDeps(cfg, pool, log, get, cs, prov, local, api)
		if err != nil {
			log.Error("collect jobs are handed back to Python: cannot wire the Go pipeline", "err", err)
			return out
		}
	}
	if d != nil && cs.GoCollect {
		out[collect.JobTypeCollect] = func(ctx context.Context, c *queue.Claim) error { return collect.HandleCollect(ctx, *d, c) }
	}
	if d != nil && cs.GoBulkEnrich {
		out[collect.JobTypeBulkEnrich] = func(ctx context.Context, c *queue.Claim) error { return collect.HandleBulkEnrich(ctx, *d, c) }
	}
	if cs.NativeProbe && cfg.Executor.SourceHealthExecutor() == "go" {
		pw := o.web
		if pw == nil && web != nil {
			pw = collect.LiveWeb{W: web}
		}
		if pw != nil {
			hd := leadjobs.Deps{Pool: pool, Getenv: os.Getenv, Executor: cfg.Executor.SourceHealthExecutor, Log: log}
			if o.health != nil {
				hd = o.health()
			}
			hd.Py = collect.NativeProber{Web: pw}
			out[leadjobs.JobType] = leadjobs.Handler(hd)
			log.Info("source_health_check probes run in process (GO_NATIVE_SOURCE_PROBE)")
		}
	}
	return out
}

// errNoBrowser: the pipeline needs the browser service for search and pages.
var errNoBrowser = errors.New("BROWSER_SERVICE_URL / BROWSER_SERVICE_TOKEN not set")

// liveCollectDeps wires the Go pipeline: leadweb over the browser service, the scrapers, the
// global source switches and health gate, the email steps of internal/emailverify, providers
// through the provider-call caller (in process for GO_LOCAL_PROVIDERS names), an LLM client per
// workspace accounting usage to it, and the leadgen progress bus.
func liveCollectDeps(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, get func(string) string,
	cs config.CollectSettings, prov collect.ProviderCaller, local *secrets.Resolver, api *credentials.Client) (*collect.Deps, *leadweb.Web, error) {
	sc := config.ScrapeFrom(get)
	if sc.BrowserURL == "" || sc.BrowserToken == "" {
		return nil, nil, errNoBrowser
	}
	sw, err := workerLeadweb(cfg, get) // the worker's one leadweb (shared rate limits and cache)
	if err != nil {
		return nil, nil, err
	}
	bc, web := sw.Browser, sw.Web
	env, err := secrets.NewEnvelope(secrets.Config{
		Provider: cfg.SecretsProvider, MasterKey: cfg.SecretsMasterKey, SecretKey: cfg.SecretKey, AppEnv: cfg.AppEnv,
		VaultAddr: cfg.VaultAddr, VaultToken: cfg.VaultToken, VaultTokenFile: cfg.VaultTokenFile,
		VaultTransitKey: cfg.VaultTransitKey, VaultNamespace: cfg.VaultNamespace, VaultCACert: cfg.VaultCACert,
	}, nil)
	if err != nil {
		return nil, nil, err
	}
	reg, err := settings.Load()
	if err != nil {
		return nil, nil, err
	}
	llmCfg, err := config.LLMFrom(get)
	if err != nil {
		return nil, nil, err
	}
	app := &settings.App{Store: secrets.NewPgStore(pool), Env: env, Secret: reg.SecretSet(), Getenv: os.Getenv}
	httpc := egress.NewOperatorClient(cfg.EgressAllowHosts)
	var py emailverify.ProviderCaller
	if prov != nil {
		py = prov
	}
	mail := emailverify.NewLive(emailverify.LiveConfig{Pool: pool, AppEnv: cfg.AppEnv, EgressAllow: cfg.EgressAllowHosts,
		HTTP: httpc, Py: py, SMTPDisabled: get("LEADGEN_SMTP_VERIFY") == "0", HeloHost: get("LEADGEN_SMTP_HELO"),
		MailFrom: get("LEADGEN_SMTP_MAIL_FROM"),
		Keys: func(ctx context.Context, ws, envVar string) (string, error) {
			if local != nil {
				v, _, err := local.Get(ctx, ws, envVar)
				return v, err
			}
			if api != nil {
				return api.Get(ctx, ws, envVar)
			}
			return "", nil
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
	log.Info("collect pipeline wired", "collect", cs.GoCollect, "bulk_enrich", cs.GoBulkEnrich, "native_probe", cs.NativeProbe,
		"provider_caller", prov != nil)
	return d, web, nil
}
