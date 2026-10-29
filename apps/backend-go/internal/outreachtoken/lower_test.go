package outreachtoken

import (
	"math"
	"testing"
	"time"
)

// Expected values recorded from Python str.strip().lower().
func TestPyLowerStripSigmaMatchesPython(t *testing.T) {
	cases := map[string]string{
		"aΣ.b": "aσ.b",
		"aΣ":   "aς",
		"Σ":    "σ",
		"aΣb":  "aσb",
		"a.Σ":  "a.ς",
		"aΣ.":  "aς.",
		"aΣ'b": "aσ'b",
		"世Σ":   "σ",
		"aΣ́":  "aς́",
		"aΣ́b": "aσ́b",
		"ΑΣ":   "ας",
		"a Σ":  "a σ",
		"a1Σ":  "a1σ",
		"aΣ·b": "aσ·b",
		"aΣ:b": "aσ:b",
	}
	cases["世Σ"] = "世σ"
	for in, want := range cases {
		if got := pyLowerStrip(in); got != want {
			t.Errorf("%+q: got %+q want %+q", in, got, want)
		}
	}
}

func TestHugeExpClampedLikePython(t *testing.T) {
	s := NewSigner([]byte("k"), "", 1, func() time.Time { return time.Unix(2_000_000_000, 0) })
	for _, c := range []struct {
		raw string
		ok  bool
	}{{"1e300", true}, {"-1e300", false}, {"9.3e18", true}, {"-9.3e18", false}} {
		if _, ok := s.Verify(signed(s, `{"email":"a@b.c","exp":`+c.raw+`,"seq":"","ws":"w"}`), false); ok != c.ok {
			t.Errorf("exp %s: ok=%v want %v", c.raw, ok, c.ok)
		}
	}
	if n, _ := pyInt([]byte("1e300")); n != math.MaxInt64 {
		t.Errorf("clamp high: %d", n)
	}
	if n, _ := pyInt([]byte("-1e300")); n != math.MinInt64 {
		t.Errorf("clamp low: %d", n)
	}
}

func signed(s *Signer, payload string) string {
	body := b64.EncodeToString([]byte(payload))
	return body + "." + s.sign(body)
}
