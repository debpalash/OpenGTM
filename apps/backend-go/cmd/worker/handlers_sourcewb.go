package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collect"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/progress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourcewb"
)

// sourcewbOpts are the source_workbook collaborators; the zero value is the production wiring minus the collect
// pipeline (collection queries then fail).
type sourcewbOpts struct {
	getenv    func(string) string
	collector sourcewb.Collector
	gate      sourcewb.LeadGate
	people    sourcewb.PeopleFinder
	// collectDeps is the worker's shared collect pipeline for the collection-query kind (nil: not wired).
	collectDeps *collect.Deps
}

// sourcewbHandlers returns the plan 15 source_workbook handler, registered unconditionally since the cutover
// (GO_EXECUTOR_SOURCE_WORKBOOK is retired). A source kind this worker lacks the seam for (collection query without
// the collect pipeline, i.e. no browser service; people_search without a crosslinked provider; crm_import without a
// secret resolver) fails permanently.
func sourcewbHandlers(pool *pgxpool.Pool, log *slog.Logger, prog progress.Publisher, hook automationhook.Hook,
	local *secrets.Resolver, o sourcewbOpts) map[string]runner {
	queue.AllowJobType(sourcewb.JobType)
	get := o.getenv
	if get == nil {
		get = config.Getenv
	}
	s, err := config.LoadSourceWb(get)
	if err != nil {
		log.Error("source_workbook jobs will fail: bad setting", "err", err)
		return map[string]runner{sourcewb.JobType: refuseJob(sourcewb.JobType, "source_workbook settings invalid")}
	}
	d := sourcewb.Deps{Pool: pool, Hook: hook, Progress: prog, Log: log,
		CRMImportEnabled: s.CRMImportEnabled, PeopleSearchEnabled: s.PeopleSearchEnabled,
		Collector: o.collector, Gate: o.gate, People: o.people}
	if d.People == nil {
		d.People = sourcewb.CrossLinkedFinder{}
	}
	if d.Collector == nil && o.collectDeps != nil {
		d.Collector = sourcewb.CollectCollector{Deps: o.collectDeps}
	}
	if d.Collector == nil {
		log.Warn("source_workbook collection queries will fail: the collect pipeline is not wired")
	}
	if d.Gate == nil {
		d.Gate = sourcewb.SplitGate{Validate: sourcewb.CollectValidateLight, Fit: sourcewb.AccountFitFunc}
	}
	if local != nil {
		d.CRM = sourcewb.CRMClients{HubSpot: &crm.HubSpot{Secrets: local}, Salesforce: &crm.Salesforce{Secrets: local}}
	}
	return map[string]runner{sourcewb.JobType: func(ctx context.Context, c *queue.Claim) error {
		err := sourcewb.Handle(ctx, d, c)
		if errors.Is(err, sourcewb.ErrUnsupported) {
			return &queue.PermanentError{Err: err}
		}
		return err
	}}
}
