package wbcrud

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// exportVectors mirrors testdata/wbcrud/export_vectors.json, generated from the real Python csv module and a
// verbatim copy of export_workbook_csv's safe_value by legacy/python/scripts/gen_export_vectors.py.
type exportVectors struct {
	Values []struct {
		JSON string `json:"json"`
		Safe string `json:"safe"`
	} `json:"values"`
	RowsJSON [][]string `json:"rows_json"`
	RowsCSV  string     `json:"rows_csv"`
	OddRows  [][]string `json:"odd_rows"`
	OddCSV   string     `json:"odd_csv"`
	Names    []struct {
		Name     string `json:"name"`
		Filename string `json:"filename"`
	} `json:"names"`
}

func loadExportVectors(t *testing.T) exportVectors {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/wbcrud/export_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v exportVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func decodeCell(t *testing.T, raw string) any {
	t.Helper()
	v, err := decodeDocument([]byte(raw), true)
	if err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return v
}

func TestExportEscapesFormulaPrefixes(t *testing.T) {
	vec := loadExportVectors(t)
	for _, c := range vec.Values {
		if got := ExportSafeValue(decodeCell(t, c.JSON)); got != c.Safe {
			t.Errorf("ExportSafeValue(%s) = %q, want %q", c.JSON, got, c.Safe)
		}
	}
	// The injection prefix set is exactly = + - @ TAB CR, tested on the first character only.
	for _, p := range []string{"=", "+", "-", "@", "\t", "\r"} {
		if got := ExportSafeValue(p + "x"); got != "'"+p+"x" {
			t.Errorf("prefix %q not escaped: %q", p, got)
		}
	}
	for _, s := range []string{" =x", "x=", "\n=x", "'=x", "0"} {
		if got := ExportSafeValue(s); got != s {
			t.Errorf("%q changed to %q", s, got)
		}
	}
}

func TestExportCSVLineEncodingMatchesPython(t *testing.T) {
	vec := loadExportVectors(t)
	var buf bytes.Buffer
	for _, row := range vec.RowsJSON {
		cells := make([]string, len(row))
		for i, r := range row {
			cells[i] = ExportSafeValue(decodeCell(t, r))
		}
		if err := writeCSVRow(&buf, cells); err != nil {
			t.Fatal(err)
		}
	}
	if buf.String() != vec.RowsCSV {
		t.Errorf("rows differ\n got %q\nwant %q", buf.String(), vec.RowsCSV)
	}
	buf.Reset()
	for _, row := range vec.OddRows {
		if err := writeCSVRow(&buf, row); err != nil {
			t.Fatal(err)
		}
	}
	if buf.String() != vec.OddCSV {
		t.Errorf("odd rows differ\n got %q\nwant %q", buf.String(), vec.OddCSV)
	}
}

func TestExportFilenameMatchesPython(t *testing.T) {
	for _, c := range loadExportVectors(t).Names {
		if got := ExportFilename(c.Name); got != c.Filename {
			t.Errorf("ExportFilename(%q) = %q, want %q", c.Name, got, c.Filename)
		}
	}
}

func TestParseRowIDsLikePydanticInt(t *testing.T) {
	good := map[string]int64{"7": 7, " 7 ": 7, "1.0": 1, "1_0": 10, "+4": 4, "-3": -3, "0010": 10, "2.000": 2}
	for in, want := range good {
		got, ok := ParseIntLax(in)
		if !ok || got != want {
			t.Errorf("ParseIntLax(%q) = %d,%v want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "x", "1e2", "1.5", "true", "_1", "1_", "1__0", "0x10", "١", "--1", "1.0.0", "1."} {
		if _, ok := ParseIntLax(in); ok {
			t.Errorf("ParseIntLax(%q) accepted", in)
		}
	}
	if n, ok := ParseIntLax("99999999999999999999"); !ok || n != 0 {
		t.Errorf("out-of-range int must parse and map to a never-matching id, got %d,%v", n, ok)
	}
}

func TestPyDumpsDefaultSeparators(t *testing.T) {
	v := decodeCell(t, `{"a":[1,2.50,"é"],"b":{"c":null,"d":true},"e":[],"f":{}}`)
	if got, want := pyDumps(v, false), `{"a": [1, 2.5, "é"], "b": {"c": null, "d": true}, "e": [], "f": {}}`; got != want {
		t.Errorf("got %s want %s", got, want)
	}
	if got, want := pyDumps(v, true), `{"a":[1,2.5,"é"],"b":{"c":null,"d":true},"e":[],"f":{}}`; got != want {
		t.Errorf("compact got %s want %s", got, want)
	}
}

func TestObjStringsUsePythonEscaping(t *testing.T) {
	o := NewObj()
	o.Set("k", "a\u2028b\u2029c\x7f<>&\"\\\n\x01\b\f\té")
	b, _ := o.MarshalJSON()
	want := "{\"k\":\"a\u2028b\u2029c\x7f<>&\\\"\\\\\\n\\u0001\\b\\f\\té\"}"
	if string(b) != want {
		t.Errorf("got %s want %s", b, want)
	}
	if !strings.Contains(string(b), "\u2028") {
		t.Error("U+2028 must stay raw like Python's ensure_ascii=False")
	}
}
