package api

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autopilot"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/copilot"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/entities"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadpeople"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// copilotTools are the plan 15 WP-COPILOT-TOOLS collaborators of the native chat engine, built once per
// handler: people research / verification / contact enrichment (internal/leadpeople), the autopilot plan
// store and run memory (Redis when the API has it: an approval may reach another API process than the draft).
type copilotTools struct {
	people   *leadpeople.Service
	executor *leadpeople.ContactExecutor
	plans    autopilot.PlanStore
	memory   autopilot.Memory
	urlCheck func(string) error // enrich_lead's lead-website egress check
}

func newCopilotTools(d Deps, s autopilot.Settings) *copilotTools {
	getenv := d.getenv()
	allow := (&leadUtils{d: d}).allowHosts()
	ct := &copilotTools{executor: &leadpeople.ContactExecutor{Pool: d.Pool}}
	if d.Redis != nil {
		ct.plans = &autopilot.RedisPlanStore{R: d.Redis, TTL: func(ctx context.Context) time.Duration { return autopilot.PlanTTL(ctx, s) }}
		ct.memory = &autopilot.RedisMemory{R: d.Redis}
	} else {
		d.logger().Warn("copilot: no Redis; autopilot plans are kept in this API process only")
	}

	appEnv := d.Config.AppEnv
	if appEnv == "" {
		appEnv = getenv("APP_ENV")
	}
	web := d.leadweb()
	ct.urlCheck = func(raw string) error { return egress.CheckURLFor(appEnv, allow, raw, true) }

	var sec providerrun.Secrets = noSecrets{}
	if d.Envelope != nil && d.Pool != nil {
		sec = secrets.NewResolver(d.Envelope, secrets.NewPgStore(d.Pool), getenv)
	}
	var operatorAllow []string
	for _, h := range strings.Split(getenv("EGRESS_ALLOW_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			operatorAllow = append(operatorAllow, h)
		}
	}
	runner := providerrun.New(providerrun.Deps{Pool: d.Pool, Secrets: sec, OperatorAllow: operatorAllow, Log: d.Log,
		ManifestHTTP: egress.NewClientForEnv(appEnv, allow), URLCheck: ct.urlCheck})
	ct.people = &leadpeople.Service{
		Identity: leadpeople.WikidataIdentity{Client: egress.NewOperatorClient(allow)},
		Search:   web.Search,
		Discover: leadpeople.RunnerDiscoverer{Runner: runner},
		Verify:   leadpeople.DepsVerifier{Deps: (&leadUtils{d: d}).verifyDeps()},
	}
	if d.Pool != nil {
		// Paid contact lookups are ledgered (review wave 3b M4); without a pool none run.
		ct.people.Spend = leadpeople.LedgerSpend{Pool: d.Pool}
	}
	return ct
}

// apply sets the tool collaborators on one turn's engine (the search cache is salted per workspace).
func (ct *copilotTools) apply(e *copilot.Engine, ws string) {
	if ct == nil {
		return
	}
	svc := *ct.people
	svc.Workspace = ws
	e.People = copilot.LeadPeople{Service: &svc, Executor: ct.executor}
	e.ResolvePerson = resolvePerson
	e.Plans, e.Memory = ct.plans, ct.memory
	// enrich_lead runs the in-process website_scraper / ddg_email providers (registry; GO_LOCAL_PROVIDERS gate).
	e.Providers, e.URLCheck = registryProviders{}, ct.urlCheck
}

func resolvePerson(ctx context.Context, tx pgx.Tx, ws string, o copilot.PersonObservation) (string, error) {
	id, _, err := entities.ResolvePerson(ctx, tx, ws, entities.PersonObservation{Name: o.Name, Company: o.Company,
		Source: o.Source, Title: o.Title, CompanyDomain: o.CompanyDomain, LinkedInURL: o.LinkedInURL, Email: o.Email,
		EvidenceURL: o.EvidenceURL, LegacyIDs: o.LegacyIDs})
	return id, err
}
