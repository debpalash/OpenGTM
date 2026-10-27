package sourcewb

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadpeople"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

type fakeCL struct{ got []leadpeople.PeopleQuery }

func (f *fakeCL) FindPeopleByTitles(_ context.Context, q leadpeople.PeopleQuery) (leadpeople.PeopleFound, error) {
	f.got = append(f.got, q)
	return leadpeople.PeopleFound{People: []map[string]any{{"name": "Jane", "title": "CTO", "linkedin": "https://linkedin.com/in/j",
		"evidence_url": "https://e"}}, SearchesUsed: 2, SearchFailures: 1}, nil
}

// buildQuery mirrors how the crosslinked provider appends ` "<geo>"` to each title query.
func providerQuery(q leadpeople.PeopleQuery, title string) string {
	s := `site:linkedin.com/in "` + q.Company + `" "` + title + `"`
	if q.Geo != "" {
		s += ` "` + q.Geo + `"`
	}
	return s
}

func TestCrossLinkedFinderMapsTheQuery(t *testing.T) {
	cl := &fakeCL{}
	f := CrossLinkedFinder{CL: cl}
	if !f.Available() {
		t.Fatal("an injected finder is available")
	}
	res, err := f.FindPeopleByTitles(context.Background(), PeopleQuery{Company: "Acme", Titles: []string{"CTO"},
		Seniority: "senior", Geo: "Berlin", MaxPeople: 2, MaxSearches: 7})
	if err != nil {
		t.Fatal(err)
	}
	want := PeopleResult{People: []Person{{Name: "Jane", Title: "CTO", LinkedIn: "https://linkedin.com/in/j", EvidenceURL: "https://e"}},
		SearchesUsed: 2, SearchFailures: 1}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("result %+v", res)
	}
	q := cl.got[0]
	// Python: site:linkedin.com/in "Acme" "CTO" "senior" "Berlin"
	if got := providerQuery(q, "CTO"); got != `site:linkedin.com/in "Acme" "CTO" "senior" "Berlin"` {
		t.Fatalf("query text %s", got)
	}
	if q.MaxPeople != 2 || q.MaxSearches != 7 || q.ResultsPerSearch != 10 {
		t.Fatalf("limits %+v", q)
	}
	for _, c := range []struct{ sen, geo, want string }{{"senior", "", "senior"}, {"", "Berlin", "Berlin"}, {"", "", ""}} {
		cl.got = nil
		_, _ = f.FindPeopleByTitles(context.Background(), PeopleQuery{Company: "A", Seniority: c.sen, Geo: c.geo})
		if cl.got[0].Geo != c.want {
			t.Errorf("seniority %q geo %q -> %q", c.sen, c.geo, cl.got[0].Geo)
		}
	}
}

func TestCrossLinkedFinderUnregistered(t *testing.T) {
	f := CrossLinkedFinder{Name: "no-such-crosslinked-provider"}
	if f.Available() {
		t.Fatal("unregistered provider must be unavailable")
	}
	if _, err := f.FindPeopleByTitles(context.Background(), PeopleQuery{}); !errors.Is(err, leadpeople.ErrCrossLinkedUnavailable) {
		t.Fatalf("err %v", err)
	}
	if ready(f) {
		t.Fatal("an unavailable finder makes people_search unsupported")
	}
}

func TestSupportedKinds(t *testing.T) {
	col := func(kv ...any) wbcrud.Column { return wbcrud.MakeColumn(kv...) }
	var d Deps
	for _, c := range []wbcrud.Column{col("id", "s", "type", "source"), col("id", "s", "type", "source", "kind", "people_search"),
		col("id", "s", "type", "source", "kind", "crm_import")} {
		if d.SupportedKind(c) {
			t.Errorf("no seams: %v must be unsupported", c.Keys())
		}
	}
	d.Collector = nil
	d.Gate = SplitGate{Fit: AccountFitFunc} // validator not wired yet
	if d.SupportedKind(col("id", "s", "type", "source")) {
		t.Error("collection needs a collector and a validator")
	}
	d.CRM = CRMClients{}
	if !d.SupportedKind(col("id", "s", "type", "source", "source", obj("kind", "CRM_Import"))) {
		t.Error("crm_import (nested kind, any case) with CRM clients is supported")
	}
}
