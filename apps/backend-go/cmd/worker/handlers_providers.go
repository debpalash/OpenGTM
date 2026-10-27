package main

import (
	"log/slog"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/poller"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rpclocal"
)

// providerRPC wires the in-process providers (plan 15 WP-PROVIDERS): it installs the GO_LOCAL_PROVIDERS gate
// (providerrun) and the providers/scrape seams, and returns the provider-call caller for signal_scan and the
// poller (listed names in process, the rest on py). nil means neither a worker nor a listed name: callers keep
// their "Python unavailable" path. sec resolves the LinkedIn session for staffspy.
func providerRPC(cfg config.Config, log *slog.Logger, py *pyworker.Client, sec rpclocal.Secrets) (poller.PyCaller, error) {
	var pc rpclocal.PyCaller
	if py != nil { // never a typed-nil client behind the interface
		pc = py
	}
	c, err := rpclocal.Configure(rpclocal.Setup{AppEnv: cfg.AppEnv, EgressAllow: cfg.EgressAllowHosts, Py: pc, Secrets: sec, Log: log})
	if err != nil || c == nil {
		return nil, err
	}
	return c, nil
}
