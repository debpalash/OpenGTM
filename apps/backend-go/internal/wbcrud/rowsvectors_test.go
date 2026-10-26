package wbcrud

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// rowVectors mirrors testdata/wbcrud/rows_vectors.json, generated from the real
// Python by legacy/python/scripts/gen_rows_vectors.py.
type rowVectors struct {
	Domains    []struct{ In, Out string } `json:"domains"`
	Companies  []struct{ In, Out string } `json:"companies"`
	Identities []struct {
		Row json.RawMessage `json:"row"`
		Out string          `json:"out"`
	} `json:"identities"`
	Dumps []struct {
		Value  json.RawMessage `json:"value"`
		Plain  string          `json:"plain"`
		Sorted string          `json:"sorted"`
	} `json:"dumps"`
	Changed []struct {
		Data   json.RawMessage `json:"data"`
		Fields json.RawMessage `json:"fields"`
		Out    json.RawMessage `json:"out"`
	} `json:"changed"`
	Downstream []struct {
		Set      string            `json:"set"`
		Columns  []json.RawMessage `json:"columns"`
		Changed  []string          `json:"changed"`
		All      []string          `json:"all"`
		Eligible []string          `json:"eligible"`
	} `json:"downstream"`
	Topo []struct {
		Set     string            `json:"set"`
		Columns []json.RawMessage `json:"columns"`
		Out     []string          `json:"out"`
	} `json:"topo"`
}

func loadRowVectors(t *testing.T) rowVectors {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/wbcrud/rows_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v rowVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func docObj(t *testing.T, raw []byte) *Obj {
	t.Helper()
	doc, err := decodeDocument(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	o, ok := doc.(*Obj)
	if !ok {
		t.Fatalf("not an object: %s", raw)
	}
	return o
}

func TestLeadDomainNormaliserMatchesPython(t *testing.T) {
	for _, c := range loadRowVectors(t).Domains {
		if got := normalizeLeadDomain(c.In); got != c.Out {
			t.Errorf("normalize_domain(%q) = %q, python %q", c.In, got, c.Out)
		}
	}
}

func TestLeadCompanyNormaliserMatchesPython(t *testing.T) {
	for _, c := range loadRowVectors(t).Companies {
		if got := normalizeLeadCompany(c.In); got != c.Out {
			t.Errorf("normalize_company(%q) = %q, python %q", c.In, got, c.Out)
		}
	}
}

func TestRowIdentityMatchesPython(t *testing.T) {
	for _, c := range loadRowVectors(t).Identities {
		if got := RowIdentity(docObj(t, c.Row)); got != c.Out {
			t.Errorf("identity(%s) = %q, python %q", c.Row, got, c.Out)
		}
	}
}

func TestPyDumpsMatchesPythonJSONDumps(t *testing.T) {
	for _, c := range loadRowVectors(t).Dumps {
		doc, err := decodeDocument(c.Value, true)
		if err != nil {
			t.Fatal(err)
		}
		if got := PyDumps(doc, false); got != c.Plain {
			t.Errorf("dumps(%s)\n got %s\nwant %s", c.Value, got, c.Plain)
		}
		if got := PyDumps(doc, true); got != c.Sorted {
			t.Errorf("dumps(%s, sort_keys)\n got %s\nwant %s", c.Value, got, c.Sorted)
		}
	}
}

func TestChangedRowFieldsMatchesPython(t *testing.T) {
	for i, c := range loadRowVectors(t).Changed {
		got := ChangedRowFields(docObj(t, c.Data), docObj(t, c.Fields))
		want := docObj(t, c.Out)
		if !reflect.DeepEqual(got.Keys(), want.Keys()) {
			t.Errorf("case %d keys %v, python %v", i, got.Keys(), want.Keys())
			continue
		}
		for _, k := range want.Keys() {
			g, _ := got.Get(k)
			w, _ := want.Get(k)
			if PyDumps(g, true) != PyDumps(w, true) {
				t.Errorf("case %d key %s: %s vs %s", i, k, PyDumps(g, true), PyDumps(w, true))
			}
		}
	}
}

func parseCols(t *testing.T, raws []json.RawMessage) []Column {
	t.Helper()
	b, _ := json.Marshal(raws)
	cols, err := ParseColumns(b)
	if err != nil {
		t.Fatal(err)
	}
	return cols
}

func ids(cols []Column) []string {
	out := []string{}
	for _, c := range cols {
		s, _ := c.Get("id").(string)
		out = append(out, s)
	}
	return out
}

func TestTopoSortMatchesPython(t *testing.T) {
	for _, c := range loadRowVectors(t).Topo {
		if got := ids(TopoSortColumns(parseCols(t, c.Columns))); !reflect.DeepEqual(got, c.Out) {
			t.Errorf("%s: %v, python %v", c.Set, got, c.Out)
		}
	}
}

func TestDownstreamColumnsMatchPython(t *testing.T) {
	for _, c := range loadRowVectors(t).Downstream {
		cols := parseCols(t, c.Columns)
		if got := ids(DownstreamColumns(cols, c.Changed, nil)); !reflect.DeepEqual(got, c.All) {
			t.Errorf("%s %v: %v, python %v", c.Set, c.Changed, got, c.All)
		}
		if got := ids(DownstreamColumns(cols, c.Changed, ReactiveEligible)); !reflect.DeepEqual(got, c.Eligible) {
			t.Errorf("%s %v eligible: %v, python %v", c.Set, c.Changed, got, c.Eligible)
		}
	}
}
