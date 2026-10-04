package pycompat

import (
	"encoding/json"
	"os"
	"testing"
)

type yamlCase struct {
	Name           string  `json:"name"`
	Input          string  `json:"input"`
	LoadOK         bool    `json:"load_ok"`
	LoadError      string  `json:"load_error"`
	Str            *string `json:"str"`
	CanonicalOK    bool    `json:"canonical_ok"`
	Canonical      string  `json:"canonical"`
	CanonicalError string  `json:"canonical_error"`
	PrettyASCII    string  `json:"pretty_ascii"`
}

type jsonCase struct {
	Name      string `json:"name"`
	Input     string `json:"input"`
	OK        bool   `json:"ok"`
	Canonical string `json:"canonical"`
	Str       string `json:"str"`
}

// TestPythonParity replays fixtures produced by testdata/gen_yaml_parity.py
// (PyYAML safe_load + json.dumps) against the Go implementation.
func TestPythonParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		YAML []yamlCase `json:"yaml"`
		JSON []jsonCase `json:"json"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.YAML) < 40 {
		t.Fatalf("parity corpus too small: %d", len(fx.YAML))
	}
	for _, c := range fx.YAML {
		t.Run("yaml/"+c.Name, func(t *testing.T) {
			v, err := LoadYAML([]byte(c.Input))
			if !c.LoadOK {
				if err == nil {
					t.Fatalf("python rejected (%s) but Go loaded %s", c.LoadError, Repr(v))
				}
				return
			}
			if err != nil {
				t.Fatalf("python loaded but Go failed: %v", err)
			}
			if c.Str != nil && Str(v) != *c.Str {
				t.Errorf("str() mismatch\n go: %s\n py: %s", Str(v), *c.Str)
			}
			got, err := Canonical(v)
			if !c.CanonicalOK {
				if err == nil {
					t.Fatalf("python canonical failed (%s) but Go produced %s", c.CanonicalError, got)
				}
				if err.Error() != c.CanonicalError {
					t.Errorf("canonical error mismatch\n go: %s\n py: %s", err, c.CanonicalError)
				}
				return
			}
			if err != nil {
				t.Fatalf("canonical: %v", err)
			}
			if string(got) != c.Canonical {
				t.Errorf("canonical mismatch\n go: %s\n py: %s", got, c.Canonical)
			}
			if c.PrettyASCII != "" {
				pretty, err := Dumps(v, DumpOptions{SortKeys: true, Indent: 2, EnsureASCII: true})
				if err != nil {
					t.Fatal(err)
				}
				if string(pretty) != c.PrettyASCII {
					t.Errorf("indent=2 mismatch\n go: %s\n py: %s", pretty, c.PrettyASCII)
				}
			}
		})
	}
	for _, c := range fx.JSON {
		t.Run("json/"+c.Name, func(t *testing.T) {
			v, err := LoadJSON([]byte(c.Input))
			if !c.OK {
				if err == nil {
					t.Fatalf("python rejected but Go parsed %s", Repr(v))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := Canonical(v)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.Canonical {
				t.Errorf("canonical mismatch\n go: %s\n py: %s", got, c.Canonical)
			}
			if Str(v) != c.Str {
				t.Errorf("str mismatch\n go: %s\n py: %s", Str(v), c.Str)
			}
		})
	}
}

func TestFloatRepr(t *testing.T) {
	cases := map[float64]string{
		0.1: "0.1", 1: "1.0", 1e16: "1e+16", 1e15: "1000000000000000.0", 0.0001: "0.0001",
		0.00001: "1e-05", 1.5e300: "1.5e+300", -2.5: "-2.5", 123456789.125: "123456789.125",
	}
	for f, want := range cases {
		if got := FloatRepr(f); got != want {
			t.Errorf("FloatRepr(%v) = %s, want %s", f, got, want)
		}
	}
}

func TestMapPythonKeyEquality(t *testing.T) {
	m := NewMap()
	_ = m.Set(int64(1), "a")
	_ = m.Set(1.0, "b")
	_ = m.Set(true, "c")
	if m.Len() != 1 {
		t.Fatalf("len = %d", m.Len())
	}
	if v, _ := m.Get(int64(1)); v != "c" {
		t.Fatalf("got %v", v)
	}
	if _, ok := m.Keys()[0].(int64); !ok {
		t.Fatal("first key object must be kept")
	}
	if err := m.Set([]any{1}, "x"); err == nil {
		t.Fatal("list key must be unhashable")
	}
}
