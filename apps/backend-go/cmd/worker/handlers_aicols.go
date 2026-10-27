package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/aicol"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/playbooks"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// wireAICols applies GO_NATIVE_AI_COLUMNS (plan 15 WP-AICOLS): the run_workbook executor runs
// ai_formula, research and agent cells and the ai_formula batch pre-pass in process, and the
// returned researcher runs playbook research steps in process. It returns nil (the Python
// worker paths stay) when the flag is off or the LLM settings cannot be wired.
func wireAICols(d *workbook.Deps, cfg config.Config, pool *pgxpool.Pool, log *slog.Logger) playbooks.Researcher {
	ac := config.LoadAICols(os.Getenv)
	for _, p := range ac.Problems {
		log.Error("invalid AI column setting", "problem", p)
	}
	if !ac.Native {
		return nil
	}
	forWS, err := aiColsDeps(cfg, pool, log)
	if err != nil {
		log.Error("GO_NATIVE_AI_COLUMNS is on but the LLM settings cannot be wired; AI columns stay on the Python worker",
			"err", err)
		return nil
	}
	d.AICols = &workbook.AIColumns{For: forWS}
	log.Info("AI columns run in process (GO_NATIVE_AI_COLUMNS)")
	return playbooks.NewNativeResearcher(forWS, playbooks.StepTimeout())
}

// aiColsDeps builds the per-workspace column collaborators: operator settings as the Python
// _read_setting saw them (stored app setting, else environment), an LLM client accounting
// usage to the workspace, leadweb search and page fetch over the browser service, and the
// strict egress guard for model-chosen URLs.
func aiColsDeps(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger) (func(context.Context, string) aicol.Deps, error) {
	env, err := secrets.NewEnvelope(secrets.Config{
		Provider: cfg.SecretsProvider, MasterKey: cfg.SecretsMasterKey, SecretKey: cfg.SecretKey, AppEnv: cfg.AppEnv,
		VaultAddr: cfg.VaultAddr, VaultToken: cfg.VaultToken, VaultTokenFile: cfg.VaultTokenFile,
		VaultTransitKey: cfg.VaultTransitKey, VaultNamespace: cfg.VaultNamespace, VaultCACert: cfg.VaultCACert,
	}, nil)
	if err != nil {
		return nil, err
	}
	reg, err := settings.Load()
	if err != nil {
		return nil, err
	}
	llmCfg, err := config.LLMFrom(os.Getenv)
	if err != nil {
		return nil, err
	}
	app := &settings.App{Store: secrets.NewPgStore(pool), Env: env, Secret: reg.SecretSet(), Getenv: os.Getenv}
	httpc := egress.NewOperatorClient(cfg.EgressAllowHosts)
	var web *leadweb.Web
	var bc *browserclient.Client
	if sc := config.ScrapeFrom(os.Getenv); sc.BrowserURL != "" {
		lw, err := config.LeadwebFrom(os.Getenv)
		if err != nil {
			return nil, err
		}
		// Internal service on the browser network: a plain client; target URLs are guarded
		// by Go first and re-checked per hop by the service.
		bc = browserclient.New(sc.BrowserURL, sc.BrowserToken.Reveal(), nil)
		web = leadweb.New(lw, bc, cfg.AppEnv, cfg.EgressAllowHosts)
	} else {
		log.Warn("BROWSER_SERVICE_URL is not set: research columns run without web search and page fetch")
	}
	appEnv, allow := cfg.AppEnv, cfg.EgressAllowHosts
	return func(_ context.Context, ws string) aicol.Deps {
		d := aicol.Deps{
			LLM: llm.New(llm.Options{Settings: app, HTTP: httpc, Usage: &llm.PgUsage{Pool: pool}, WorkspaceID: ws,
				Config: llmCfg, Registry: reg, Log: log}),
			Settings: app,
			URLCheck: func(u string) error { return egress.CheckURLFor(appEnv, allow, u, true) },
		}
		if web != nil {
			d.Search = aicol.WebSearch{Searcher: web.Search, Workspace: ws}
			d.Pages = aicol.WebPages{Fetcher: web.Fetch, Extractor: bc}
		}
		return d
	}, nil
}
