package sourcewb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collect"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadpeople"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// availability is implemented by seams that can be present but not ready (a provider that
// is not registered in this process); an unavailable seam makes its kind unsupported.
type availability interface{ Available() bool }

// CrossLinkedFinder is the production PeopleFinder: the crosslinked provider's
// leadpeople.CrossLinked (WP-PROVIDERS registers it as providers.Get("crosslinked")).
type CrossLinkedFinder struct {
	CL   leadpeople.CrossLinked // nil: look up Name in the provider registry per call
	Name string                 // "" = "crosslinked"
}

func (f CrossLinkedFinder) finder() leadpeople.CrossLinked {
	if f.CL != nil {
		return f.CL
	}
	name := f.Name
	if name == "" {
		name = "crosslinked"
	}
	if p, ok := providers.Get(name); ok {
		if cl, ok := p.(leadpeople.CrossLinked); ok {
			return cl
		}
	}
	return nil
}

// Available reports whether a crosslinked finder is registered.
func (f CrossLinkedFinder) Available() bool { return f.finder() != nil }

// FindPeopleByTitles maps the people_search call onto leadpeople's query. Python's
// find_people_by_titles appends ` "<seniority>"` then ` "<geo>"` to each title query;
// leadpeople.PeopleQuery has only Geo (appended as ` "<geo>"`), so the seniority term
// rides in front of it with the closing/opening quotes between them, which yields the
// same query text. results_per_search=10 is the people_search default.
func (f CrossLinkedFinder) FindPeopleByTitles(ctx context.Context, q PeopleQuery) (PeopleResult, error) {
	cl := f.finder()
	if cl == nil {
		return PeopleResult{}, leadpeople.ErrCrossLinkedUnavailable
	}
	geo := q.Geo
	if q.Seniority != "" {
		geo = q.Seniority
		if q.Geo != "" {
			geo += `" "` + q.Geo
		}
	}
	res, err := cl.FindPeopleByTitles(ctx, leadpeople.PeopleQuery{Company: q.Company, Titles: q.Titles, Geo: geo,
		MaxPeople: q.MaxPeople, MaxSearches: q.MaxSearches, ResultsPerSearch: 10, Delay: 200 * time.Millisecond})
	if err != nil {
		return PeopleResult{}, err
	}
	out := PeopleResult{SearchesUsed: res.SearchesUsed, SearchFailures: res.SearchFailures}
	for _, p := range res.People {
		out.People = append(out.People, Person{Name: orStr(p["name"]), Title: orStr(p["title"]),
			LinkedIn: orStr(p["linkedin"]), EvidenceURL: orStr(p["evidence_url"])})
	}
	return out, nil
}

// AccountFitFunc evaluates account fit with leadpeople.EvaluateAccountFit.
func AccountFitFunc(lead, brief *wbcrud.Obj) (*wbcrud.Obj, error) {
	l, _ := plain(lead).(map[string]any)
	b, _ := plain(brief).(map[string]any)
	fit := leadpeople.EvaluateAccountFit(l, b, time.Now().UTC())
	raw, err := marshalAny(fit)
	if err != nil {
		return nil, err
	}
	return wbcrud.ParseObject(raw)
}

// SplitGate composes a LeadGate from the light validator (internal/collect) and an
// account-fit function.
type SplitGate struct {
	Validate func(lead *wbcrud.Obj) (bool, string)
	Fit      func(lead, brief *wbcrud.Obj) (*wbcrud.Obj, error)
}

func (g SplitGate) ValidateLight(lead *wbcrud.Obj) (bool, string) { return g.Validate(lead) }
func (g SplitGate) AccountFit(lead, brief *wbcrud.Obj) (*wbcrud.Obj, error) {
	return g.Fit(lead, brief)
}

// Available: the validator comes from internal/collect, which may not be wired yet.
func (g SplitGate) Available() bool { return g.Validate != nil && g.Fit != nil }

// CollectCollector is the production Collector: internal/collect's Submitter (JobRunner.submit)
// over the workspace's runner.
type CollectCollector struct{ Deps *collect.Deps }

// Submit runs the collection inline and returns its job id.
func (c CollectCollector) Submit(ctx context.Context, ws, query string) (string, error) {
	return collect.Submitter{Pool: c.Deps.Pool, Runner: c.Deps.For(ws), Emit: c.Deps.Emit}.Submit(ctx, ws, query)
}

// ErrCollectionInFlight means an earlier attempt's collection under the same key has not finished (a crashed
// worker). Starting another would pay twice, so the attempt fails and the queue's retry budget decides.
var ErrCollectionInFlight = errors.New("sourcewb: the collection of an earlier attempt has not finished")

// KeyedJobID is the collection job id of an idempotency key: stable per (workspace, key).
func KeyedJobID(ws, key string) string {
	sum := sha256.Sum256([]byte(ws + "\x00" + key))
	return "sw" + hex.EncodeToString(sum[:15])
}

// SubmitKeyed is Submit made idempotent. A ledger row that already exists under the key's job id and is
// terminal (done, failed or cancelled) is returned as it is, with no new run; otherwise the run uses
// that id (Submitter.NewID), so the next attempt finds it.
func (c CollectCollector) SubmitKeyed(ctx context.Context, ws, query, key string) (string, error) {
	id := KeyedJobID(ws, key)
	st, err := collect.PgLedger{Pool: c.Deps.Pool, WS: ws}.Status(ctx, id)
	if err != nil {
		return "", err
	}
	switch st {
	case "":
	case "done", "completed", "failed", "cancelled":
		return id, nil
	default:
		return "", ErrCollectionInFlight
	}
	return collect.Submitter{Pool: c.Deps.Pool, Runner: c.Deps.For(ws), Emit: c.Deps.Emit, NewID: func() string { return id }}.Submit(ctx, ws, query)
}

// Available: the collect pipeline is wired.
func (c CollectCollector) Available() bool { return c.Deps != nil && c.Deps.For != nil }

// CollectValidateLight is lead_validator.validate_lead_light over internal/collect.
func CollectValidateLight(lead *wbcrud.Obj) (bool, string) {
	raw, err := lead.MarshalJSON()
	if err != nil {
		return false, "invalid_lead"
	}
	var l collect.Lead
	if err := json.Unmarshal(raw, &l); err != nil {
		return false, "invalid_lead"
	}
	return collect.ValidateLeadLight(&l)
}
