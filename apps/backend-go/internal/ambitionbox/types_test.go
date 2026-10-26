package ambitionbox

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

type vectors struct {
	Slugify  []struct{ Input, Slug string } `json:"slugify"`
	Industry []struct{ Input, Slug string } `json:"industry_slug"`
	Aliases  map[string]string              `json:"aliases"`
	Company  []struct {
		Name      string          `json:"name"`
		Card      json.RawMessage `json:"card"`
		Value     json.RawMessage `json:"value"`
		ErrorType string          `json:"error_type"`
	} `json:"company_from_api"`
	Job []struct {
		Name      string          `json:"name"`
		Job       json.RawMessage `json:"job"`
		Value     json.RawMessage `json:"value"`
		ErrorType string          `json:"error_type"`
	} `json:"job_from_api"`
	Fetch []struct {
		Name         string          `json:"name"`
		Args         json.RawMessage `json:"args"`
		ResponseBody json.RawMessage `json:"response_body"`
		Requests     []struct {
			Method string          `json:"method"`
			URL    string          `json:"url"`
			JSON   json.RawMessage `json:"json"`
		} `json:"requests"`
		Value     json.RawMessage `json:"value"`
		ErrorType string          `json:"error_type"`
		Error     string          `json:"error"`
	} `json:"fetch_company_page"`
	Collect []struct {
		Name       string            `json:"name"`
		Args       map[string]any    `json:"args"`
		Scripted   []json.RawMessage `json:"scripted_pages"`
		FetchCalls []map[string]any  `json:"fetch_calls"`
		Value      map[string]any    `json:"value"`
		Error      string            `json:"error"`
	} `json:"collect_companies"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/ambitionbox.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func mustObj(t *testing.T, raw []byte) *wbcrud.Obj {
	t.Helper()
	o, err := wbcrud.ParseObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// canon re-encodes recorded JSON the way Obj.MarshalJSON writes it.
func canon(t *testing.T, raw []byte) string {
	t.Helper()
	o, err := wbcrud.ParseObject(raw)
	if err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	b, _ := o.MarshalJSON()
	return string(b)
}

func TestIndustrySlugVectors(t *testing.T) {
	v := loadVectors(t)
	if len(v.Slugify) == 0 || len(v.Industry) == 0 {
		t.Fatal("no vectors")
	}
	for _, c := range v.Slugify {
		if got := slugify(c.Input); got != c.Slug {
			t.Errorf("slugify(%q)=%q want %q", c.Input, got, c.Slug)
		}
	}
	for _, c := range v.Industry {
		if got := industrySlug(c.Input); got != c.Slug {
			t.Errorf("industrySlug(%q)=%q want %q", c.Input, got, c.Slug)
		}
	}
	if len(industryAliases) != len(v.Aliases) {
		t.Errorf("alias count %d want %d", len(industryAliases), len(v.Aliases))
	}
	for k, want := range v.Aliases {
		if industryAliases[k] != want {
			t.Errorf("alias %q=%q want %q", k, industryAliases[k], want)
		}
	}
}

func TestCompanyFromAPIVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Company {
		co, err := CompanyFromAPI(mustObj(t, c.Card))
		if c.ErrorType != "" {
			if err == nil {
				t.Errorf("%s: want error (%s)", c.Name, c.ErrorType)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		b, _ := co.ToDict().MarshalJSON()
		if string(b) != canon(t, c.Value) {
			t.Errorf("%s:\n got %s\nwant %s", c.Name, b, canon(t, c.Value))
		}
	}
}

func TestJobFromAPIVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Job {
		j, err := JobFromAPI(mustObj(t, c.Job))
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		b, _ := j.ToDict().MarshalJSON()
		if string(b) != canon(t, c.Value) {
			t.Errorf("%s:\n got %s\nwant %s", c.Name, b, canon(t, c.Value))
		}
	}
}
