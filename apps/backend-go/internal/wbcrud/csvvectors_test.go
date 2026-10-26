package wbcrud

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// csvVectors mirrors testdata/wbcrud/csv_vectors.json, generated from the real
// Python by legacy/python/scripts/gen_csv_vectors.py.
type csvVectors struct {
	Headers []struct {
		In, Norm, Key, Target string
	} `json:"headers"`
	LeadFields []string          `json:"lead_fields"`
	LowerAlnum [][2]any          `json:"lower_alnum"`
	Casefold   [][2]any          `json:"casefold"`
	Title      [][2]any          `json:"title"`
	TitleWords [][2]string       `json:"title_words"`
	Scenarios  []json.RawMessage `json:"scenarios"`
}

func loadCSVVectors(t *testing.T) csvVectors {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/wbcrud/csv_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v csvVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func cpOf(t *testing.T, v any) rune {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("bad code point %v", v)
	}
	return rune(f)
}

func TestHeaderNormalisationVectors(t *testing.T) {
	v := loadCSVVectors(t)
	for _, h := range v.Headers {
		if got := NormalizeHeader(h.In); got != h.Norm {
			t.Errorf("NormalizeHeader(%q) = %q, want %q", h.In, got, h.Norm)
		}
		if got := ColumnKey(h.In); got != h.Key {
			t.Errorf("ColumnKey(%q) = %q, want %q", h.In, got, h.Key)
		}
	}
}

// Python lowercases with the full Unicode mapping before the [^a-z0-9] split, so
// every code point must project onto the same ASCII letters/digits.
func TestHeaderLowerMappingEveryCodePoint(t *testing.T) {
	v := loadCSVVectors(t)
	want := map[rune]string{}
	for _, e := range v.LowerAlnum {
		want[cpOf(t, e[0])] = e[1].(string)
	}
	bad := 0
	for cp := rune(0); cp < 0x20000; cp++ {
		if cp >= 0xD800 && cp <= 0xDFFF {
			continue
		}
		got := strings.ReplaceAll(NormalizeHeader(string(cp)), " ", "")
		if got != want[cp] {
			if bad++; bad <= 10 {
				t.Errorf("U+%04X: alnum projection %q, want %q", cp, got, want[cp])
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d code points differ", bad)
	}
}

func TestInferTargetVectors(t *testing.T) {
	v := loadCSVVectors(t)
	p := NewCSVPlanner(v.LeadFields)
	for _, h := range v.Headers {
		if got := p.InferTarget(h.In); got != h.Target {
			t.Errorf("InferTarget(%q) = %q, want %q", h.In, got, h.Target)
		}
	}
}

func TestPyCasefoldEveryCodePoint(t *testing.T) {
	v := loadCSVVectors(t)
	want := map[rune]string{}
	for _, e := range v.Casefold {
		want[cpOf(t, e[0])] = e[1].(string)
	}
	bad := 0
	for cp := rune(0); cp < 0x20000; cp++ {
		if cp >= 0xD800 && cp <= 0xDFFF {
			continue
		}
		w, ok := want[cp]
		if !ok {
			w = string(cp)
		}
		if got := pyCasefold(string(cp)); got != w {
			if bad++; bad <= 10 {
				t.Errorf("casefold U+%04X = %q, want %q", cp, got, w)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d code points differ", bad)
	}
}

func TestPyTitleEveryCodePoint(t *testing.T) {
	v := loadCSVVectors(t)
	want := map[rune]string{}
	for _, e := range v.Title {
		want[cpOf(t, e[0])] = e[1].(string)
	}
	bad := 0
	for cp := rune(0); cp < 0x20000; cp++ {
		if cp >= 0xD800 && cp <= 0xDFFF {
			continue
		}
		w, ok := want[cp]
		if !ok {
			w = string(cp)
		}
		if got := pyTitle(string(cp)); got != w {
			if bad++; bad <= 10 {
				t.Errorf("title U+%04X = %q, want %q", cp, got, w)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d code points differ", bad)
	}
	for _, w := range v.TitleWords {
		if got := pyTitle(w[0]); got != w[1] {
			t.Errorf("pyTitle(%q) = %q, want %q", w[0], got, w[1])
		}
	}
}

type csvScenario struct {
	Rows     json.RawMessage `json:"rows"`
	Columns  json.RawMessage `json:"columns"`
	Mapping  json.RawMessage `json:"mapping"`
	Create   bool            `json:"create_columns"`
	FileName string          `json:"file_name"`
	System   string          `json:"source_system"`
	OutRows  json.RawMessage `json:"out_rows"`
	OutCols  json.RawMessage `json:"out_columns"`
	Added    json.RawMessage `json:"added"`
	Resolved json.RawMessage `json:"resolved"`
	Analysis json.RawMessage `json:"analysis"`
}

// canon re-renders vector JSON through the ordered decoder so both sides use
// the same key order and number text.
func canon(t *testing.T, raw []byte) string {
	t.Helper()
	doc, err := decodeDocument(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := writeValue(&b, doc); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func scenarioInputs(t *testing.T, sc csvScenario) ([]*Obj, []Column, CSVMapping) {
	t.Helper()
	doc, err := decodeDocument(sc.Rows, true)
	if err != nil {
		t.Fatal(err)
	}
	var rows []*Obj
	for _, e := range doc.([]any) {
		rows = append(rows, e.(*Obj))
	}
	cols, err := ParseColumns(sc.Columns)
	if err != nil {
		t.Fatal(err)
	}
	var mapping CSVMapping
	if string(sc.Mapping) != "null" {
		md, err := decodeDocument(sc.Mapping, true)
		if err != nil {
			t.Fatal(err)
		}
		mapping = CSVMapping{}
		mo := md.(*Obj)
		for _, k := range mo.Keys() {
			val, _ := mo.Get(k)
			if s, ok := val.(string); ok {
				mapping[k] = &s
			} else {
				mapping[k] = nil
			}
		}
	}
	return rows, cols, mapping
}

func TestPrepareAndAnalyzeVectors(t *testing.T) {
	v := loadCSVVectors(t)
	p := NewCSVPlanner(v.LeadFields)
	if len(v.Scenarios) < 100 {
		t.Fatalf("too few scenarios: %d", len(v.Scenarios))
	}
	for i, raw := range v.Scenarios {
		var sc csvScenario
		if err := json.Unmarshal(raw, &sc); err != nil {
			t.Fatal(err)
		}
		rows, cols, mapping := scenarioInputs(t, sc)
		prep := p.Prepare(rows, cols, mapping, sc.Create)
		check := func(what string, got any, want json.RawMessage) {
			var b bytes.Buffer
			if err := writeValue(&b, got); err != nil {
				t.Fatal(err)
			}
			if w := canon(t, want); b.String() != w {
				t.Errorf("scenario %d %s:\n got %s\nwant %s", i, what, b.String(), w)
			}
		}
		outRows := make([]any, len(prep.Rows))
		for k, r := range prep.Rows {
			outRows[k] = r
		}
		outCols := make([]any, len(prep.Columns))
		for k, c := range prep.Columns {
			outCols[k] = c
		}
		added := make([]any, len(prep.Added))
		for k, c := range prep.Added {
			added[k] = c
		}
		check("rows", outRows, sc.OutRows)
		check("columns", outCols, sc.OutCols)
		check("added", added, sc.Added)
		check("resolved", prep.ResolvedObj(), sc.Resolved)
		check("analysis", p.Analyze(rows, prep, sc.FileName, sc.System), sc.Analysis)
	}
}
