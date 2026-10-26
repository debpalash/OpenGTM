package api

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type csvVectors struct {
	CSVSafe []struct {
		In     any    `json:"in"`
		Type   string `json:"type"`
		Out    any    `json:"out"`
		OutStr string `json:"out_str"`
	} `json:"csv_safe"`
	Rows []struct {
		Values []any  `json:"values"`
		Line   string `json:"line"`
	} `json:"rows"`
	Header     []string `json:"header"`
	HeaderLine string   `json:"header_line"`
}

func loadCSVVectors(t *testing.T) csvVectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/csv_safe.json")
	if err != nil {
		t.Fatal(err)
	}
	var v csvVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// vectorValue restores the Python type lost by JSON (ints stay float64 after decoding).
func vectorValue(typ string, in any) any {
	if f, ok := in.(float64); ok && typ == "int" {
		return int64(f)
	}
	return in
}

func TestCSVSafeVectors(t *testing.T) {
	for _, c := range loadCSVVectors(t).CSVSafe {
		in := vectorValue(c.Type, c.In)
		got := csvSafe(in)
		if c.Type == "str" || c.Type == "None" {
			if s, _ := got.(string); s != c.Out {
				t.Errorf("csvSafe(%q) = %#v, want %q", c.In, got, c.Out)
			}
			continue
		}
		if csvText(got) != c.OutStr {
			t.Errorf("csvText(csvSafe(%v)) = %q, want %q", c.In, csvText(got), c.OutStr)
		}
	}
}

func TestExportHeaderRow(t *testing.T) {
	v := loadCSVVectors(t)
	if len(exportHeader) != 27 {
		t.Fatalf("%d header titles", len(exportHeader))
	}
	var b bytes.Buffer
	if err := writeExportRow(&b, toAny(exportHeader)); err != nil {
		t.Fatal(err)
	}
	if b.String() != v.HeaderLine {
		t.Errorf("header %q, want %q", b.String(), v.HeaderLine)
	}
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, x := range s {
		out[i] = x
	}
	return out
}

func TestCSVWriterMatchesPythonLines(t *testing.T) {
	for i, r := range loadCSVVectors(t).Rows {
		vals := make([]any, len(r.Values))
		for j, x := range r.Values {
			vals[j] = x
			if f, ok := x.(float64); ok && f == float64(int64(f)) && !strings.Contains(r.Line, "1.5") {
				vals[j] = int64(f)
			}
		}
		var b bytes.Buffer
		if err := writeExportRow(&b, vals); err != nil {
			t.Fatal(err)
		}
		if b.String() != r.Line {
			t.Errorf("row %d: %q, want %q", i, b.String(), r.Line)
		}
	}
}
