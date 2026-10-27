package sourcewb

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

type pureVectors struct {
	BuildQuery []struct {
		ICP   json.RawMessage `json:"icp"`
		Query string          `json:"query"`
	} `json:"build_query"`
	PreviewSource []struct {
		ICP      json.RawMessage `json:"icp"`
		Channels json.RawMessage `json:"channels"`
		Out      json.RawMessage `json:"out"`
	} `json:"preview_source"`
	PreviewPeople []struct {
		Col json.RawMessage `json:"col"`
		Out json.RawMessage `json:"out"`
	} `json:"preview_people"`
	PersonIdentity []struct {
		Name, Company, Linkedin, ID string
	} `json:"person_identity"`
	PeopleRowData []struct {
		Company string            `json:"company"`
		Person  map[string]string `json:"person"`
		Data    json.RawMessage   `json:"data"`
	} `json:"people_row_data"`
	CRMLimit []struct {
		Cfg   json.RawMessage `json:"cfg"`
		Limit int             `json:"limit"`
	} `json:"crm_limit"`
}

func loadVectors(t *testing.T) (pureVectors, []json.RawMessage) {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Pure      pureVectors       `json:"pure"`
		Scenarios []json.RawMessage `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v.Pure, v.Scenarios
}

// doc decodes JSON the way stored workbook JSON is decoded (ordered, json.Number).
func doc(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	if len(bytes.TrimSpace(raw)) == 0 || string(raw) == "null" {
		return nil
	}
	o, err := wbcrud.ParseObject(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return o
}

// sameJSON compares two JSON documents semantically; ordered additionally compares key order.
func sameJSON(t *testing.T, got any, want json.RawMessage, ordered bool) (bool, string) {
	t.Helper()
	g, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if err := json.Unmarshal(g, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		return false, string(g)
	}
	if ordered {
		var cw bytes.Buffer
		_ = json.Compact(&cw, want)
		if gw, _ := wbcrud.ParseObject(g); gw != nil {
			ww, _ := wbcrud.ParseObject(cw.Bytes())
			if ww != nil && !reflect.DeepEqual(gw.Keys(), ww.Keys()) {
				return false, "key order: " + string(g)
			}
		}
	}
	return true, ""
}

func TestBuildQueryVectors(t *testing.T) {
	v, _ := loadVectors(t)
	for _, c := range v.BuildQuery {
		if got := BuildQuery(doc(t, c.ICP)); got != c.Query {
			t.Errorf("build_query(%s) = %q want %q", c.ICP, got, c.Query)
		}
	}
}

func TestPreviewSourceVectors(t *testing.T) {
	v, _ := loadVectors(t)
	for _, c := range v.PreviewSource {
		got := PreviewSource(doc(t, c.ICP), doc(t, c.Channels))
		if ok, g := sameJSON(t, got, c.Out, true); !ok {
			t.Errorf("preview_source(%s):\n got %s\nwant %s", c.Channels, g, c.Out)
		}
	}
}

func TestPreviewPeopleVectors(t *testing.T) {
	v, _ := loadVectors(t)
	for _, c := range v.PreviewPeople {
		o := doc(t, c.Col).(*wbcrud.Obj)
		m := map[string]any{}
		for _, k := range o.Keys() {
			m[k], _ = o.Get(k)
		}
		if ok, g := sameJSON(t, PreviewPeopleSearch(m), c.Out, true); !ok {
			t.Errorf("preview_people(%s):\n got %s\nwant %s", c.Col, g, c.Out)
		}
	}
}

func TestPersonIdentityAndRowVectors(t *testing.T) {
	v, _ := loadVectors(t)
	for _, c := range v.PersonIdentity {
		if got := PersonIdentity(c.Name, c.Company, c.Linkedin); got != c.ID {
			t.Errorf("person_identity(%q,%q,%q) = %s want %s", c.Name, c.Company, c.Linkedin, got, c.ID)
		}
	}
	for _, c := range v.PeopleRowData {
		p := Person{Name: c.Person["name"], Title: c.Person["title"], LinkedIn: c.Person["linkedin"]}
		if ok, g := sameJSON(t, PersonRowData(c.Company, p), c.Data, true); !ok {
			t.Errorf("_row_data(%q, %v):\n got %s\nwant %s", c.Company, c.Person, g, c.Data)
		}
	}
}

func TestCRMLimitVectors(t *testing.T) {
	v, _ := loadVectors(t)
	for _, c := range v.CRMLimit {
		m := map[string]any{}
		if o, ok := doc(t, c.Cfg).(*wbcrud.Obj); ok {
			for _, k := range o.Keys() {
				m[k], _ = o.Get(k)
			}
		}
		if got := CRMEffectiveLimit(m); got != c.Limit {
			t.Errorf("effective_limit(%s) = %d want %d", c.Cfg, got, c.Limit)
		}
	}
}
