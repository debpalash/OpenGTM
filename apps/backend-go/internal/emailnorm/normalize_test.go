package emailnorm

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

type vector struct {
	Input      string   `json:"input"`
	Keys       []string `json:"keys"`
	Normalized string   `json:"normalized"`
}

func loadVectors(t testing.TB) []vector {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []vector `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) < 50 {
		t.Fatalf("too few vectors: %d", len(f.Cases))
	}
	return f.Cases
}

func TestNormalizeMatchesVectors(t *testing.T) {
	for _, c := range loadVectors(t) {
		if got := Normalize(c.Input); got != c.Normalized {
			t.Errorf("Normalize(%q) = %q, want %q", c.Input, got, c.Normalized)
		}
	}
}

func TestMatchKeysMatchesVectors(t *testing.T) {
	for _, c := range loadVectors(t) {
		got := MatchKeys(c.Input)
		if len(got) == 0 && len(c.Keys) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.Keys) {
			t.Errorf("MatchKeys(%q) = %q, want %q", c.Input, got, c.Keys)
		}
	}
}

func TestNormalizeNoAtSign(t *testing.T) {
	if got := Normalize("  Not-An-Address "); got != "not-an-address" {
		t.Errorf("got %q", got)
	}
	if got := Normalize(""); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestNormalizeIDNAFailureFallsBack(t *testing.T) {
	// empty label in the middle is an IDNA error in Python; falls back to lower-case.
	if got := Normalize("U@A..COM"); got != "u@a..com" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("é", 70) + ".com"
	if got := Normalize("u@" + long); got != "u@"+long {
		t.Errorf("got %q", got)
	}
}

func TestGmailCollapseOnlyForGmail(t *testing.T) {
	if got := MatchKeys("A.B+tag@Gmail.com"); !reflect.DeepEqual(got, []string{"a.b+tag@gmail.com", "ab@gmail.com"}) {
		t.Errorf("gmail: %q", got)
	}
	if got := MatchKeys("a.b+tag@googlemail.com"); len(got) != 2 {
		t.Errorf("googlemail: %q", got)
	}
	if got := MatchKeys("a.b+tag@example.com"); !reflect.DeepEqual(got, []string{"a.b+tag@example.com"}) {
		t.Errorf("other: %q", got)
	}
	if got := MatchKeys("+x@gmail.com"); !reflect.DeepEqual(got, []string{"+x@gmail.com"}) {
		t.Errorf("empty collapsed local: %q", got)
	}
	if got := MatchKeys(""); len(got) != 0 {
		t.Errorf("empty: %q", got)
	}
}

func FuzzNormalize(f *testing.F) {
	for _, c := range loadVectors(f) {
		f.Add(c.Input)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			t.Skip()
		}
		n := Normalize(s)
		_ = MatchKeys(s)
		_ = n
	})
}

func TestNormalizeIdempotentOnVectors(t *testing.T) {
	for _, c := range loadVectors(t) {
		n := Normalize(c.Input)
		if Normalize(n) != n {
			t.Errorf("not idempotent for %q: %q -> %q", c.Input, n, Normalize(n))
		}
	}
}
