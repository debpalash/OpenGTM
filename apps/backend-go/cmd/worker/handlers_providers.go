package main

import (
	"log/slog"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/poller"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rpclocal"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// providerRPC wires the in-process providers (plan 15 WP-PROVIDERS): it installs the providers/scrape seams and
// returns the provider-call caller for signal_scan, the poller, collect and email verification. Every provider runs
// in process since the cutover (GO_LOCAL_PROVIDERS is permanently "all"; no Python worker is configured). nil means
// rpclocal configured nothing. sec resolves the LinkedIn session for staffspy.
//
// The Go providers run through run (the worker's providerrun.Runner: settings, cache, stats ledger) and
// share the worker's one leadweb.
func providerRPC(cfg config.Config, log *slog.Logger, sec rpclocal.Secrets, run workbook.ProviderRunner) (poller.PyCaller, error) {
	sw, err := workerLeadweb(cfg, nil)
	if err != nil {
		return nil, err
	}
	setup := rpclocal.Setup{AppEnv: cfg.AppEnv, EgressAllow: cfg.EgressAllowHosts, Secrets: sec, Log: log,
		Web: sw.Web, Browser: sw.Browser}
	if r, ok := run.(rpclocal.Runner); ok && r != nil {
		setup.Runner = r
	}
	c, err := rpclocal.Configure(setup)
	if err != nil || c == nil {
		return nil, err
	}
	return c, nil
}
