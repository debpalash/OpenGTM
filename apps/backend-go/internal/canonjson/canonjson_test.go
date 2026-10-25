package canonjson_test

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

type vectors struct {
	Canon []struct {
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
		Sorted   string          `json:"sorted"`
		Unsorted string          `json:"unsorted"`
	} `json:"canon"`
}

func TestCanonJSONVectors(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/canonjson_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Canon) < 30 {
		t.Fatalf("need >=30 vectors, have %d", len(v.Canon))
	}
	for _, c := range v.Canon {
		in, err := canonjson.Decode(c.Input)
		if err != nil {
			t.Fatalf("%s: decode: %v", c.Name, err)
		}
		for _, sortKeys := range []bool{true, false} {
			want := c.Unsorted
			if sortKeys {
				want = c.Sorted
			}
			got, err := canonjson.Marshal(in, sortKeys)
			if err != nil {
				t.Fatalf("%s: %v", c.Name, err)
			}
			if !sortKeys && multiKey(in) {
				// Go maps carry no insertion order, so unsorted output of
				// multi-key maps is emitted key-sorted; compare modulo order.
				wv, err := canonjson.Decode([]byte(want))
				if err != nil {
					t.Fatal(err)
				}
				want2, _ := canonjson.Marshal(wv, true)
				want = string(want2)
			}
			if string(got) != want {
				t.Errorf("%s sort=%v:\n got %s\nwant %s", c.Name, sortKeys, got, want)
			}
		}
	}
}

func multiKey(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if len(x) > 1 {
			return true
		}
		for _, e := range x {
			if multiKey(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if multiKey(e) {
				return true
			}
		}
	}
	return false
}

func TestFloatRepr(t *testing.T) {
	a, b := 0.1, 0.2
	cases := map[float64]string{
		0.05: "0.05", a + b: "0.30000000000000004", 1e-07: "1e-07", 1e16: "1e+16", 1e15: "1000000000000000.0",
		100: "100.0", math.Copysign(0, -1): "-0.0", 5e-324: "5e-324", 0.0001: "0.0001", 0.00001: "1e-05",
		1.5e-5: "1.5e-05", 1e22: "1e+22", 123456789012345680: "1.2345678901234568e+17", 1e100: "1e+100",
	}
	for f, want := range cases {
		got, err := canonjson.Marshal(f, false)
		if err != nil || string(got) != want {
			t.Errorf("%v: got %q err %v want %q", f, got, err, want)
		}
	}
}

func TestRejects(t *testing.T) {
	bad := []any{math.NaN(), math.Inf(1), math.Inf(-1), 5, int32(1), struct{}{}, []int{1}, map[string]string{}, "bad\xffutf8",
		map[string]any{"k": math.NaN()}, []any{uint8(1)}, map[string]any{"\xff": 1}}
	for _, b := range bad {
		for _, s := range []bool{true, false} {
			if out, err := canonjson.Marshal(b, s); err == nil {
				t.Errorf("%#v accepted: %s", b, out)
			}
		}
	}
}

func TestMarshalDoesNotEscapeHTMLAndNilIsNull(t *testing.T) {
	got, _ := canonjson.Marshal(map[string]any{"a": "<>&", "n": nil, "e": []any{}}, true)
	if !bytes.Equal(got, []byte(`{"a":"<>&","e":[],"n":null}`)) {
		t.Fatal(string(got))
	}
	if !strings.Contains(string(mustMarshal(t, "😀")), `\ud83d\ude00`) {
		t.Fatal("surrogate pair")
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	b, err := canonjson.Marshal(v, false)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeRejectsLoneSurrogatesAndBadUTF8(t *testing.T) {
	for _, bad := range []string{`"\ud800"`, `"\udc00"`, `"\ud800x"`, `"\ud800A"`, `["\udbff"]`, `{"\udc00":1}`, "\"\xff\"", `"\u12"`} {
		if v, err := canonjson.Decode([]byte(bad)); err == nil {
			t.Errorf("%q accepted: %#v", bad, v)
		}
	}
	for _, good := range []string{`"😀"`, `"\\ud800"`, `"\\\\u"`, `"é"`} {
		if _, err := canonjson.Decode([]byte(good)); err != nil {
			t.Errorf("%q rejected: %v", good, err)
		}
	}
}
