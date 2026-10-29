package pyjson

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

type corpusCase struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Compact string `json:"compact"`
	Indent2 string `json:"indent2"`
}

func TestDumpsCorpusMatchesPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []corpusCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 8 {
		t.Fatalf("corpus too small: %d", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			got, err := Dumps(json.RawMessage(c.Value), 0)
			if err != nil || string(got) != c.Compact {
				t.Fatalf("compact: %q err=%v want %q", got, err, c.Compact)
			}
			got, err = Dumps(json.RawMessage(c.Value), 2)
			if err != nil || string(got) != c.Indent2 {
				t.Fatalf("indent2: %q err=%v want %q", got, err, c.Indent2)
			}
		})
	}
}

func TestDumpsKeyOrderPreserved(t *testing.T) {
	o := NewObj("z", int64(1), "a", NewObj("y", []any{int64(1)}, "b", nil), "m", "x")
	got, err := Dumps(o, 0)
	if err != nil || string(got) != `{"z": 1, "a": {"y": [1], "b": null}, "m": "x"}` {
		t.Fatalf("%q %v", got, err)
	}
	got, err = Dumps(json.RawMessage(`{"z":1,"a":2,"z":3}`), 0)
	if err != nil || string(got) != `{"z": 3, "a": 2}` { // python dict: dup keeps first slot, last value
		t.Fatalf("%q %v", got, err)
	}
	got, _ = Dumps(NewObj("k", int64(1), "k", int64(2)), 0)
	if string(got) != `{"k": 2}` {
		t.Fatalf("%q", got)
	}
}

func TestDumpsEnsureASCIISurrogates(t *testing.T) {
	got, err := Dumps("\u00e9\U0001F600\u2028\x7f\x01", 0)
	if err != nil || string(got) != `"\u00e9\ud83d\ude00\u2028\u007f\u0001"` {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := Dumps("bad\xff", 0); err == nil {
		t.Fatal("invalid UTF-8 must error")
	}
}

func TestDumpsFloatRepr(t *testing.T) {
	cases := map[float64]string{1.5: "1.5", 1e16: "1e+16", 1e-5: "1e-05", 100: "100.0", 0.1: "0.1",
		math.NaN(): "NaN", math.Inf(1): "Infinity", math.Inf(-1): "-Infinity"}
	for f, want := range cases {
		got, err := Dumps(f, 0)
		if err != nil || string(got) != want {
			t.Errorf("%v: %q %v want %q", f, got, err, want)
		}
	}
	got, _ := Dumps([]any{math.NaN(), math.Inf(-1)}, 2)
	if string(got) != "[\n  NaN,\n  -Infinity\n]" {
		t.Fatalf("%q", got)
	}
}

func TestDumpsDatetimeStr(t *testing.T) {
	ist := time.FixedZone("", 5*3600+30*60)
	pst := time.FixedZone("", -8*3600)
	cases := []struct {
		t    time.Time
		want string
	}{
		{time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), `{"d": "2026-01-02 03:04:05+00:00"}`},
		{time.Date(2026, 1, 2, 3, 4, 5, 123000000, time.UTC), `{"d": "2026-01-02 03:04:05.123000+00:00"}`},
		{time.Date(2026, 1, 2, 3, 4, 5, 123456789, ist), `{"d": "2026-01-02 03:04:05.123456+05:30"}`},
		{time.Date(2026, 1, 2, 3, 4, 5, 0, pst), `{"d": "2026-01-02 03:04:05-08:00"}`},
	}
	for _, c := range cases {
		got, err := Dumps(NewObj("d", c.t), 0)
		if err != nil || string(got) != c.want {
			t.Errorf("%q %v want %q", got, err, c.want)
		}
	}
}

func TestDumpsRejectsUnsupportedTypes(t *testing.T) {
	deep := any("x")
	for i := 0; i < 300; i++ {
		deep = []any{deep}
	}
	bad := []any{int(1), int32(1), map[string]any{"a": 1}, struct{}{}, []string{"a"}, float32(1), json.Number("1"), uint(1), &Obj{Keys: []string{"a"}, Vals: map[string]any{}}, deep, json.RawMessage(`{bad`), json.RawMessage(`[1] x`), json.RawMessage("[\"\xff\"]")}
	for _, v := range bad {
		if _, err := Dumps(v, 0); err == nil {
			t.Errorf("%T %v: expected error", v, v)
		}
	}
	if got, err := Dumps((*Obj)(nil), 0); err != nil || string(got) != "null" {
		t.Errorf("nil obj: %q %v", got, err)
	}
}
