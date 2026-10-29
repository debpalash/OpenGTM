package main

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/poller"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rpclocal"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/runplan"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// providerRPC wires the in-process providers (plan 15 WP-PROVIDERS): it installs the providers/scrape seams and
// returns the provider-call caller for signal_scan, the poller, collect and email verification. Every provider runs
// in process since the cutover (GO_LOCAL_PROVIDERS is permanently "all"; no Python worker is configured). nil means
// rpclocal configured nothing. sec resolves the LinkedIn session for staffspy.
//
// The Go providers run through run (the worker's providerrun.Runner: settings, cache, stats ledger) and
// share the worker's one leadweb.
func providerRPC(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, sec rpclocal.Secrets, run workbook.ProviderRunner) (poller.PyCaller, error) {
	sw, err := workerLeadweb(cfg, nil)
	if err != nil {
		return nil, err
	}
	setup := rpclocal.Setup{AppEnv: cfg.AppEnv, EgressAllow: cfg.EgressAllowHosts, Secrets: sec, Log: log,
		Web: sw.Web, Browser: sw.Browser, Pool: pool} // Pool: the workspace spend ledger of scoped paid calls
	if r, ok := run.(rpclocal.Runner); ok && r != nil {
		setup.Runner = r
	}
	c, err := rpclocal.Configure(setup)
	if err != nil || c == nil {
		return nil, err
	}
	return c, nil
}

// workerProviderRunner is the worker's one provider runner (workbook cells, RunRows, the provider-call runner of
// signal_scan, the poller, collect and copilot): Go providers in process, and any other name from its
// signature-verified declarative manifest in the worker's connector index (built-ins or signed user connectors,
// under CONNECTOR_SIGNATURE_POLICY), with the env-aware egress client and URL guard the executor uses for
// manifests. It also wires the vendor catalog to that index (runplan.Wire), so a run the worker plans prices a
// connector by its declared cost_per_lookup, as the API's planner does. appSetting may be nil.
func workerProviderRunner(cfg config.Config, pool *pgxpool.Pool, sec providerrun.Secrets,
	appSetting func(ctx context.Context, key, def string) (string, error), log *slog.Logger) *providerrun.Runner {
	runplan.Wire(runplan.WireDeps{Index: connectorIndex, AppSetting: appSetting})
	return providerrun.New(providerrun.Deps{Pool: pool, Secrets: sec, OperatorAllow: cfg.EgressAllowHosts, Log: log,
		Manifests:    connectorIndex,
		ManifestHTTP: egress.NewClientForEnv(cfg.AppEnv, cfg.EgressAllowHosts),
		URLCheck: func(raw string) error {
			return egress.CheckURLFor(cfg.AppEnv, cfg.EgressAllowHosts, raw, true)
		},
	})
}
