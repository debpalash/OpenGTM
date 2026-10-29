package automationhook

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"testing"
)

// TestSha8OfPythonStr checks sha8/pyStr against vectors recorded from Python's events._sha8.
func TestSha8OfPythonStr(t *testing.T) {
	raw, err := os.ReadFile("testdata/sha8_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var vs []struct {
		Value any    `json:"value"`
		Sha8  string `json:"sha8"`
		Str   string `json:"str"`
	}
	if err := dec.Decode(&vs); err != nil {
		t.Fatal(err)
	}
	if len(vs) < 20 {
		t.Fatalf("only %d vectors", len(vs))
	}
	for i, v := range vs {
		val := v.Value
		if val == "__inf__" {
			val = math.Inf(1)
		}
		if got := pyStr(val); got != v.Str {
			t.Errorf("#%d pyStr(%#v)=%q want %q", i, val, got, v.Str)
		}
		if got := sha8(val); got != v.Sha8 {
			t.Errorf("#%d sha8(%#v)=%q want %q", i, val, got, v.Sha8)
		}
	}
}
