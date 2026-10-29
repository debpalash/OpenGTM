package domainintel

import (
	"encoding/json"
	"os"
	"testing"
)

func TestCleanDomainVectors(t *testing.T) {
	vec := loadVectors(t)
	if len(vec.CleanDomain) < 50 {
		t.Fatalf("only %d vectors", len(vec.CleanDomain))
	}
	for _, c := range vec.CleanDomain {
		in, ok := c.In.(string) // non-strings (null, numbers, lists, objects) are "" in Python; the Go API takes a string
		if !ok {
			if c.Out != "" {
				t.Fatalf("non-string input %v expected %q", c.In, c.Out)
			}
			continue
		}
		if got := Clean(in); got != c.Out {
			t.Errorf("Clean(%q) = %q want %q", in, got, c.Out)
		}
	}
}

// TestCleanDomainFuzzVectors replays 6000 seeded inputs (unicode labels, bidi, brackets, control characters,
// schemes, userinfo, ports) recorded from Python clean_domain by legacy/python/scripts/gen_domainintel_fuzz.py.
func TestCleanDomainFuzzVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/clean_fuzz_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		In  string `json:"in"`
		Out string `json:"out"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 5000 {
		t.Fatalf("only %d vectors", len(cases))
	}
	bad := 0
	for _, c := range cases {
		if got := Clean(c.In); got != c.Out {
			bad++
			if bad <= 15 {
				t.Errorf("Clean(%q) = %q want %q", c.In, got, c.Out)
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d/%d diffs", bad, len(cases))
	}
}

func TestCleanDomainRejectsInvalidUTF8AndHuge(t *testing.T) {
	for _, in := range []string{"a\xffb.com", string([]byte{0xc0, 0xaf}) + ".com"} {
		if got := Clean(in); got != "" {
			t.Errorf("Clean(%q) = %q", in, got)
		}
	}
	huge := make([]byte, 0, 1<<20)
	for len(huge) < 1<<20 {
		huge = append(huge, "a.b"...)
	}
	if got := Clean(string(huge)); got != "" {
		t.Fatalf("1 MiB input must clean to empty, got %d bytes", len(got))
	}
}

func TestISODateAge(t *testing.T) {
	raw, err := os.ReadFile("testdata/isodate_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		In  string `json:"in"`
		Age *int   `json:"age"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	now := testNow(t)
	bad := 0
	for _, c := range cases {
		got, ok := ageDays(c.In, now)
		if (c.Age == nil) == ok || (ok && got != *c.Age) {
			bad++
			if bad <= 15 {
				t.Errorf("ageDays(%q) = %d,%v want %v", c.In, got, ok, c.Age)
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d/%d diffs", bad, len(cases))
	}
}
