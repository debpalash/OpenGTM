package leadswrite

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"strconv"
	"testing"
)

// vector mirrors testdata/leadwrite_vectors.json (legacy/python/scripts/gen_leadwrite_vectors.py).
type vector struct {
	Class  string  `json:"class"`
	Column string  `json:"column"`
	Raw    string  `json:"raw"`
	OK     bool    `json:"ok"`
	Stored *string `json:"stored"`
	Error  string  `json:"error"`
}

func loadVectors(t testing.TB) []vector {
	t.Helper()
	b, err := os.ReadFile("../../testdata/leadwrite_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []vector
	if err := json.Unmarshal(b, &vs); err != nil {
		t.Fatal(err)
	}
	if len(vs) < 300 {
		t.Fatalf("only %d vectors", len(vs))
	}
	return vs
}

// body decodes a raw JSON value like the route does (UseNumber) plus the
// non-standard constants Python's json.loads accepts.
func body(t testing.TB, raw string) any {
	t.Helper()
	switch raw {
	case "NaN":
		return math.NaN()
	case "Infinity":
		return math.Inf(1)
	}
	d := json.NewDecoder(bytes.NewReader([]byte(raw)))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

func TestCoerceMatchesPythonVectors(t *testing.T) {
	for _, v := range loadVectors(t) {
		typ := map[string]ColType{"text_notnull": TText, "text_null": TTextNull, "int": TInt}[v.Class]
		if MutableColumns[v.Column] != typ {
			t.Fatalf("column %s class mismatch", v.Column)
		}
		got, err := coerce(v.Column, typ, body(t, v.Raw))
		if !v.OK {
			// Python 500s: Go must answer 422 (FieldError), never accept.
			if err == nil {
				t.Errorf("%s %s: Python errors (%s) but Go accepted %v", v.Class, v.Raw, v.Error, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s %s: Python stores %v but Go rejected: %v", v.Class, v.Raw, v.Stored, err)
			continue
		}
		switch g := got.(type) {
		case nil:
			if v.Stored != nil {
				t.Errorf("%s %s: Go NULL, Python %q", v.Class, v.Raw, *v.Stored)
			}
		case int32:
			if v.Stored == nil || *v.Stored != strconv.Itoa(int(g)) {
				t.Errorf("%s %s: Go %d, Python %v", v.Class, v.Raw, g, v.Stored)
			}
		case string:
			if v.Stored == nil || *v.Stored != g {
				t.Errorf("%s %s: Go %q, Python %v", v.Class, v.Raw, g, v.Stored)
			}
		default:
			t.Errorf("unexpected arg type %T", got)
		}
	}
}
