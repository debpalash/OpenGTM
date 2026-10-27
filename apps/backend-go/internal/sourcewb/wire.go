package sourcewb

import (
	"context"
	"time"

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
