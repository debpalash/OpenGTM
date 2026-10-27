package main

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collect"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/credentials"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/progress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourcewb"
)

// sourcewbOpts are test seams; the zero value is the production wiring.
type sourcewbOpts struct {
	getenv    func(string) string
	collector sourcewb.Collector
	gate      sourcewb.LeadGate
	people    sourcewb.PeopleFinder
	// collectDeps wires internal/collect for the collection-query kind (nil: not wired).
	collectDeps func(get func(string) string) (*collect.Deps, error)
}

// registerSourcewb adds the source_workbook handler to handlers (the one-line wiring in main).
// cfg, prov and api feed the collect pipeline of the collection-query kind (as registerCollect).
func registerSourcewb(handlers map[string]runner, cfg config.Config, pool *pgxpool.Pool, log *slog.Logger,
	prog progress.Publisher, hook automationhook.Hook, local *secrets.Resolver, prov collect.ProviderCaller,
	api *credentials.Client) {
	o := sourcewbOpts{}
	o.collectDeps = func(get func(string) string) (*collect.Deps, error) {
		cs, err := config.LoadCollect(get)
		if err != nil {
			return nil, err
		}
		d, _, err := liveCollectDeps(cfg, pool, log, get, cs, prov, local, api)
		return d, err
	}
	for typ, h := range sourcewbHandlers(pool, log, prog, hook, local, o) {
		handlers[typ] = h
	}
}

// sourcewbHandlers returns the plan 15 source_workbook handler. The job type is always
// claimable; a row is handed back to Python when GO_EXECUTOR_SOURCE_WORKBOOK is off or when
// this worker lacks the seam its source kind needs (collection query: the internal/collect
// pipeline, which needs the browser service;
// people_search: a registered crosslinked provider; crm_import: an in-process secret
// resolver, so CRM tokens never fall back to the process environment).
func sourcewbHandlers(pool *pgxpool.Pool, log *slog.Logger, prog progress.Publisher, hook automationhook.Hook,
	local *secrets.Resolver, o sourcewbOpts) map[string]runner {
	queue.AllowJobType(sourcewb.JobType)
	get := o.getenv
	if get == nil {
		get = os.Getenv
	}
	s, err := config.LoadSourceWb(get)
	if err != nil {
		log.Error("source_workbook jobs are handed back to Python: bad flag", "err", err)
		return map[string]runner{sourcewb.JobType: handBackInert(pool, "source_workbook flags invalid")}
	}
	if !s.GoExecutor {
		return map[string]runner{sourcewb.JobType: handBackInert(pool, "GO_EXECUTOR_SOURCE_WORKBOOK is off")}
	}
	d := sourcewb.Deps{Pool: pool, Hook: hook, Progress: prog, Log: log,
		CRMImportEnabled: s.CRMImportEnabled, PeopleSearchEnabled: s.PeopleSearchEnabled,
		Collector: o.collector, Gate: o.gate, People: o.people}
	if d.People == nil {
		d.People = sourcewb.CrossLinkedFinder{}
	}
	if d.Collector == nil && o.collectDeps != nil {
		// The collection query runs the internal/collect pipeline in process (it needs the
		// browser service); when it cannot be wired the kind is handed back to Python.
		if cd, err := o.collectDeps(get); err != nil {
			log.Warn("source_workbook collection queries are handed back to Python: collect pipeline not wired", "err", err)
		} else {
			d.Collector = sourcewb.CollectCollector{Deps: cd}
		}
	}
	if d.Gate == nil {
		d.Gate = sourcewb.SplitGate{Validate: sourcewb.CollectValidateLight, Fit: sourcewb.AccountFitFunc}
	}
	if local != nil {
		d.CRM = sourcewb.CRMClients{HubSpot: &crm.HubSpot{Secrets: local}, Salesforce: &crm.Salesforce{Secrets: local}}
	}
	handBack := handBackInert(pool, "source kind not supported by the Go worker")
	return map[string]runner{sourcewb.JobType: func(ctx context.Context, c *queue.Claim) error {
		err := sourcewb.Handle(ctx, d, c)
		if errors.Is(err, sourcewb.ErrUnsupported) {
			return handBack(ctx, c)
		}
		return err
	}}
}
