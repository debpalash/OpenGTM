package api

import (
	"os"
	"strings"
	"testing"
)

// TestIngestTokenCompareConstantTime pins the comparison primitive: equal-length hex digests go through
// crypto/subtle, a length mismatch is a plain false, and the route file never compares hashes with == / !=.
func TestIngestTokenCompareConstantTime(t *testing.T) {
	a := hashIngestToken("wbi_a")
	if !ingestHashEqual(a, hashIngestToken("wbi_a")) {
		t.Error("equal hashes not equal")
	}
	if ingestHashEqual(a, hashIngestToken("wbi_b")) || ingestHashEqual(a, a[:63]) || ingestHashEqual(a, "") {
		t.Error("different hashes compared equal")
	}
	if len(a) != 64 {
		t.Errorf("hash length %d, want 64 hex chars", len(a))
	}
	src, err := os.ReadFile("ingestroutes.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if !strings.Contains(s, "crypto/subtle") || !strings.Contains(s, "subtle.ConstantTimeCompare") {
		t.Error("ingestroutes.go must compare token hashes with crypto/subtle")
	}
	for _, bad := range []string{"TokenHash ==", "TokenHash !=", "tokenHash ==", "tokenHash !=", "== presented", "!= presented"} {
		if strings.Contains(s, bad) {
			t.Errorf("ingestroutes.go compares a token hash with %q", bad)
		}
	}
}

func TestIngestExtractToken(t *testing.T) {
	cases := []struct {
		xh, auth, want string
	}{
		{"wbi_x", "", "wbi_x"},
		{"  wbi_x  ", "Bearer wbi_y", "wbi_x"},
		{"", "Bearer wbi_y", "wbi_y"},
		{"", "bearer   wbi_y ", "wbi_y"},
		{"", "Bearer eyJhbGciOi", ""},
		{"", "Basic wbi_y", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := extractIngestToken(c.xh, c.auth); got != c.want {
			t.Errorf("extract(%q,%q) = %q, want %q", c.xh, c.auth, got, c.want)
		}
	}
}
