package collect

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func load(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

type strCase struct {
	In   string `json:"in"`
	City string `json:"city"`
	Out  any    `json:"out"`
}

func TestNamesVectors(t *testing.T) {
	var v map[string][]strCase
	load(t, "names.json", &v)
	check := func(kind string, fn func(c strCase) any) {
		if len(v[kind]) == 0 {
			t.Fatalf("%s: no vectors", kind)
		}
		for _, c := range v[kind] {
			got := fn(c)
			want := c.Out
			if xs, ok := want.([]any); ok {
				var s []string
				for _, x := range xs {
					s = append(s, x.(string))
				}
				want = s
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s(%q): got %#v want %#v", kind, c.In, got, want)
			}
		}
	}
	check("city", func(c strCase) any { return ExtractCity(c.In) })
	check("location", func(c strCase) any { return IsLocationQuery(c.In) })
	check("expand", func(c strCase) any { return ExpandQuery(c.In) })
	check("region", func(c strCase) any { return DetectRegion(c.In, c.City) })
	check("business_name", func(c strCase) any { return ExtractBusinessName(c.In) })
	check("clean_name", func(c strCase) any { return CleanName(c.In) })
	check("company_from_domain", func(c strCase) any { return CompanyFromDomain(c.In) })
	check("skip_domain", func(c strCase) any { return IsSkipDomain(c.In) })
	check("size", func(c strCase) any { return ExtractSize(c.In) })
}

func TestValidateVectors(t *testing.T) {
	var v []struct {
		Fn      string            `json:"fn"`
		Company string            `json:"company"`
		In      map[string]string `json:"in"`
		OK      bool              `json:"ok"`
		Reason  string            `json:"reason"`
		Out     map[string]string `json:"out"`
	}
	load(t, "validate.json", &v)
	if len(v) < 500 {
		t.Fatalf("only %d vectors", len(v))
	}
	for _, c := range v {
		raw, _ := json.Marshal(c.In)
		var l Lead
		_ = json.Unmarshal(raw, &l)
		l.Company = c.Company
		var ok bool
		var reason string
		if c.Fn == "full" {
			ok, reason = ValidateLead(&l)
		} else {
			ok, reason = ValidateLeadLight(&l)
		}
		got := map[string]string{"email": l.Email, "email_confidence": l.EmailConfidence, "email_provider": l.EmailProvider,
			"phone": l.Phone, "website": l.Website, "linkedin_url": l.LinkedinURL}
		if ok != c.OK || reason != c.Reason || !reflect.DeepEqual(got, c.Out) {
			t.Errorf("%s(%q, %v): got %v %q %v want %v %q %v", c.Fn, c.Company, c.In, ok, reason, got, c.OK, c.Reason, c.Out)
		}
	}
}

func TestDedupVectors(t *testing.T) {
	var v struct {
		Ratio []struct {
			A, B string
			Out  int
		} `json:"ratio"`
		Dedup []struct {
			In  []map[string]string `json:"in"`
			Out []string            `json:"out"`
		} `json:"dedup"`
	}
	load(t, "dedup.json", &v)
	for _, c := range v.Ratio {
		if got := TokenSortRatio(c.A, c.B); got != c.Out {
			t.Errorf("token_sort_ratio(%q, %q) = %d want %d", c.A, c.B, got, c.Out)
		}
	}
	for i, c := range v.Dedup {
		var leads []*Lead
		for _, d := range c.In {
			raw, _ := json.Marshal(d)
			var l Lead
			_ = json.Unmarshal(raw, &l)
			leads = append(leads, &l)
		}
		var got []string
		for _, l := range Dedupe(leads, DedupThreshold) {
			got = append(got, l.Company+"|"+l.Website+"|"+l.Email)
		}
		if len(got) != len(c.Out) || (len(got) > 0 && !reflect.DeepEqual(got, c.Out)) {
			t.Errorf("dedup case %d: got %v want %v", i, got, c.Out)
		}
	}
}

func TestRegistryVectors(t *testing.T) {
	var v struct {
		All   map[string][]string `json:"get_all_sources"`
		Build []struct {
			Source, Q, City string
			Out             []string
		} `json:"build_queries"`
	}
	load(t, "registry.json", &v)
	names := func(ss []Source) []string {
		out := []string{}
		for _, s := range ss {
			out = append(out, s.Name)
		}
		return out
	}
	for region, want := range v.All {
		var got []string
		switch region {
		case "all_disabled_too":
			got = names(AllSources("", false))
		case "None":
			got = names(AllSources("", true))
		default:
			got = names(AllSources(region, true))
		}
		if len(want) == 0 {
			want = []string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("get_all_sources(%s): got %v want %v", region, got, want)
		}
	}
	byName := map[string]Source{}
	for _, s := range registry {
		byName[s.Name] = s
	}
	for _, c := range v.Build {
		got := BuildQueries(byName[c.Source], c.Q, c.City)
		if !reflect.DeepEqual(got, c.Out) {
			t.Errorf("build_queries(%s, %q, %q) = %v want %v", c.Source, c.Q, c.City, got, c.Out)
		}
	}
}
