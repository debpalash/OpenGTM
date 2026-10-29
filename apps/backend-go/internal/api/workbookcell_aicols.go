package api

import (
	"context"
	"log/slog"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/aicol"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// aiColumns wires GO_NATIVE_AI_COLUMNS (plan 15 WP-AICOLS) into the single-cell route with the
// same collaborators the worker's run_workbook executor uses (cmd/worker wireAICols): operator
// settings, a workspace-accounted LLM client, leadweb search/fetch over the browser service and
// the strict egress guard for model-chosen URLs. Paid agent tools reserve through the executor's
// spend ledger (workbook reservedCall), keyed by the cell's identity. nil: the path stays off
// (flag off, or settings/envelope missing).
func aiColumns(d Deps, log *slog.Logger) *workbook.AIColumns {
	getenv := d.getenv()
	ac := config.LoadAICols(getenv)
	for _, p := range ac.Problems {
		log.Error("cell run: " + p)
	}
	if !ac.Native {
		return nil
	}
	if d.Registry == nil || d.Envelope == nil {
		log.Error("cell run: GO_NATIVE_AI_COLUMNS is on but settings or the secrets envelope are missing; AI cells stay unavailable")
		return nil
	}
	llmCfg, err := config.LLMFrom(getenv)
	if err != nil {
		log.Error("cell run: LLM settings invalid; AI cells stay unavailable", "err_type", errTypeName(err))
		return nil
	}
	app := &settings.App{Store: secrets.NewPgStore(d.Pool), Env: d.Envelope, Secret: d.Registry.SecretSet(), Getenv: getenv}
	httpc := egress.NewOperatorClient(d.EgressAllowHosts)
	appEnv, allow := d.Config.AppEnv, d.EgressAllowHosts
	bc, web := d.browser(), d.leadweb()
	if !bc.Configured() {
		log.Warn("cell run: BROWSER_SERVICE_URL is not set; browser rendering is unavailable; configured web API providers remain available")
	}
	return &workbook.AIColumns{For: func(_ context.Context, ws string) aicol.Deps {
		cd := aicol.Deps{
			LLM: llm.New(llm.Options{Settings: app, HTTP: httpc, Usage: &llm.PgUsage{Pool: d.Pool}, WorkspaceID: ws,
				Config: llmCfg, Registry: d.Registry, Log: log}),
			Settings: app,
			URLCheck: func(u string) error { return egress.CheckURLFor(appEnv, allow, u, true) },
		}
		if web != nil {
			if bc.Configured() || web.Search.KeyedConfigured() {
				cd.Search = aicol.WebSearch{Searcher: web.Search, Workspace: ws}
			}
			if bc.Configured() {
				cd.Pages = aicol.WebPages{Fetcher: web.Fetch, Extractor: bc}
			}
		}
		return cd
	}}
}
