package leadpeople

import (
	"context"
	"errors"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

type fakeCL struct {
	found PeopleFound
	err   error
	calls []PeopleQuery
}

func (f *fakeCL) FindPeopleByTitles(_ context.Context, q PeopleQuery) (PeopleFound, error) {
	f.calls = append(f.calls, q)
	return f.found, f.err
}

type classErr struct{ class string }

func (e classErr) Error() string      { return e.class }
func (e classErr) ErrorClass() string { return e.class }

type fakeID struct {
	res   map[string]any
	err   error
	calls int
}

func (f *fakeID) ResolveIdentity(context.Context, string) (map[string]any, error) {
	f.calls++
	return f.res, f.err
}

// idFromVector maps a recorded Wikidata script (dict, null, string or
// {"__raise__": class}) to the fake: non-dicts are Python's invalid_response.
func idFromVector(w any) *fakeID {
	m, ok := w.(map[string]any)
	if !ok {
		return &fakeID{}
	}
	if cls, ok := m["__raise__"].(string); ok {
		return &fakeID{err: classErr{cls}}
	}
	return &fakeID{res: m}
}

func TestResolveCompanyIdentityVectors(t *testing.T) {
	var cases []struct {
		Company  string `json:"company"`
		Wikidata any    `json:"wikidata"`
		Result   any    `json:"result"`
	}
	loadVec(t, "resolve.json", &cases)
	for i, c := range cases {
		id := idFromVector(c.Wikidata)
		s := &Service{Identity: id, Now: func() time.Time { return fixedNow }}
		got := s.ResolveCompanyIdentity(context.Background(), c.Company)
		sameJSON(t, "resolve["+c.Company+"]#"+string(rune('a'+i)), got, c.Result)
	}
}

func TestResearchVectors(t *testing.T) {
	var cases []struct {
		Company     string   `json:"company"`
		Function    string   `json:"function"`
		Titles      []string `json:"titles"`
		Location    string   `json:"location"`
		Limit       int      `json:"limit"`
		CrossLinked struct {
			People   []any  `json:"people"`
			Searches int    `json:"searches"`
			Failures int    `json:"failures"`
			Raise    string `json:"raise"`
		} `json:"crosslinked"`
		Wikidata any `json:"wikidata"`
		Calls    []struct {
			CompanyName      string   `json:"company_name"`
			Titles           []string `json:"titles"`
			Geo              string   `json:"geo"`
			MaxPeople        int      `json:"max_people"`
			MaxSearches      int      `json:"max_searches"`
			ResultsPerSearch int      `json:"results_per_search"`
			Delay            float64  `json:"delay"`
			CtorMaxPeople    int      `json:"ctor_max_people"`
		} `json:"calls"`
		Result any    `json:"result"`
		Raises string `json:"raises"`
	}
	loadVec(t, "research.json", &cases)
	for i, c := range cases {
		cl := &fakeCL{found: PeopleFound{People: asMaps(c.CrossLinked.People), SearchesUsed: c.CrossLinked.Searches, SearchFailures: c.CrossLinked.Failures}}
		switch c.CrossLinked.Raise {
		case "timeout":
			cl.err = context.DeadlineExceeded
		case "value":
			cl.err = errors.New("value error")
		}
		id := idFromVector(c.Wikidata)
		s := &Service{CrossLinked: cl, Identity: id, Now: func() time.Time { return fixedNow }}
		got, err := s.ResearchPeopleAtCompany(context.Background(), c.Company, c.Function,
			ResearchOptions{Titles: c.Titles, Location: c.Location, Limit: c.Limit})
		if c.Raises != "" {
			if err == nil {
				t.Errorf("case %d: want error (%s)", i, c.Raises)
			}
			continue
		}
		if err != nil {
			t.Errorf("case %d: %v", i, err)
			continue
		}
		sameJSON(t, "research#"+string(rune('a'+i)), got, c.Result)
		if len(cl.calls) != len(c.Calls) {
			t.Errorf("case %d: %d finder calls, want %d", i, len(cl.calls), len(c.Calls))
			continue
		}
		for j, w := range c.Calls {
			g := cl.calls[j]
			if g.Company != w.CompanyName || g.Geo != w.Geo || g.MaxPeople != w.MaxPeople || g.MaxSearches != w.MaxSearches ||
				g.ResultsPerSearch != w.ResultsPerSearch || g.Delay.Seconds() != w.Delay || w.CtorMaxPeople != w.MaxPeople {
				t.Errorf("case %d call %d: got %+v want %+v", i, j, g, w)
			}
			sameJSON(t, "titles", g.Titles, w.Titles)
		}
	}
}

func TestResearchNoFinder(t *testing.T) {
	s := &Service{Identity: &fakeID{res: map[string]any{"status": "not_found"}}}
	if _, err := s.ResearchPeopleAtCompany(context.Background(), "Stripe", "Sales", ResearchOptions{}); !errors.Is(err, ErrCrossLinkedUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestResearchCallerCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Service{CrossLinked: &fakeCL{err: context.Canceled}, Identity: &fakeID{}}
	if _, err := s.ResearchPeopleAtCompany(ctx, "Stripe", "Sales", ResearchOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
