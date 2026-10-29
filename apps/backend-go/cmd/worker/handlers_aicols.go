package main

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/aicol"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/playbooks"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// wireAICols wires the in-process AI columns (plan 15 WP-AICOLS; GO_NATIVE_AI_COLUMNS is permanently on since the
// cutover): the run_workbook executor runs ai_formula, research and agent cells and the ai_formula batch pre-pass in
// process, and the returned researcher runs playbook research steps. It returns nil (AI cells and playbook runs
// then fail) when the LLM settings cannot be wired.
func wireAICols(d *workbook.Deps, cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, sh *workerShared) playbooks.Researcher {
	ac := config.LoadAICols(config.Getenv)
	for _, p := range ac.Problems {
		log.Error("invalid AI column setting", "problem", p)
	}
	forWS, err := aiColsDeps(cfg, pool, log, sh)
	if err != nil {
		log.Error("the AI column LLM settings cannot be wired; AI cells and playbook runs will fail", "err", err)
		return nil
	}
	d.AICols = &workbook.AIColumns{For: forWS}
	log.Info("AI columns run in process")
	return playbooks.NewNativeResearcher(forWS, playbooks.StepTimeout())
}

// aiColsDeps builds the per-workspace column collaborators: operator settings as the Python
// _read_setting saw them (stored app setting, else environment), an LLM client accounting
// usage to the workspace (through aicol.UsageTap, so each column call settles its spend-ledger
// attempt with the observed usage), leadweb search and page fetch over the browser service, and the
// syntactic egress guard for model-chosen URLs (no DNS: the browser service resolves and
// checks every hop).
func aiColsDeps(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, sh *workerShared) (func(context.Context, string) aicol.Deps, error) {
	if sh == nil {
		return nil, errors.New("no shared settings")
	}
	app, reg, llmCfg := sh.app, sh.reg, sh.llmCfg
	httpc := egress.NewOperatorClient(cfg.EgressAllowHosts)
	// API-backed search/markdown also work without the optional browser sidecar.
	sw, err := workerLeadweb(cfg, os.Getenv)
	if err != nil {
		return nil, err
	}
	bc, web := sw.Browser, sw.Web
	if !bc.Configured() {
		log.Warn("BROWSER_SERVICE_URL is not set: browser rendering is unavailable; configured web API providers remain available")
	}
	appEnv, allow := cfg.AppEnv, cfg.EgressAllowHosts
	return func(_ context.Context, ws string) aicol.Deps {
		d := aicol.Deps{
			LLM: llm.New(llm.Options{Settings: app, HTTP: httpc, Usage: aicol.UsageTap{Next: &llm.PgUsage{Pool: pool}}, WorkspaceID: ws,
				Config: llmCfg, Registry: reg, Log: log}),
			Settings: app,
			URLCheck: func(u string) error { return egress.CheckURLFor(appEnv, allow, u, true) },
		}
		if web != nil {
			if bc.Configured() || web.Search.KeyedConfigured() {
				d.Search = aicol.WebSearch{Searcher: web.Search, Workspace: ws}
			}
			if bc.Configured() {
				d.Pages = aicol.WebPages{Fetcher: web.Fetch, Extractor: bc}
			}
			if web.Spicrawl != nil {
				d.Pages = web.Spicrawl
			}
		}
		return d
	}, nil
}
