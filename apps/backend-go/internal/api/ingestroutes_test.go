package api

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

func TestIngestUpdateScopeRejectsIdentityAndDuplicateFields(t *testing.T) {
	for _, body := range []string{
		`{"update_fields":["website"]}`,
		`{"update_fields":["Intel Score","intel score"]}`,
		`{"update_fields":["Intel Score","row_id"]}`,
		`{"update_fields":["Intel Score"],"surprise":true}`,
	} {
		r := httptest.NewRequest("POST", "/ingest-token", strings.NewReader(body))
		if _, err := parseIngestUpdateScope(r); err == nil {
			t.Errorf("accepted unsafe token scope %s", body)
		}
	}
	r := httptest.NewRequest("POST", "/ingest-token", strings.NewReader(`{"update_fields":["Intel Score","Intent Topics"]}`))
	fields, err := parseIngestUpdateScope(r)
	if err != nil || len(fields) != 2 || fields[0] != "Intel Score" {
		t.Fatalf("safe scope rejected: %v %v", fields, err)
	}
}

func TestIngestUpdatePatchKeepsOnlyGrantedFields(t *testing.T) {
	row := wbcrud.NewObj()
	row.Set("website", "https://acme.io")
	row.Set("Intel Score", 61)
	patch, err := ingestUpdatePatch(row, []string{"Intel Score"})
	if err != nil || patch.Len() != 1 {
		t.Fatalf("patch rejected: %v %v", patch, err)
	}
	if _, found := patch.Get("website"); found {
		t.Error("identity field entered update patch")
	}
	row.Set("ICP Band", nil)
	patch, err = ingestUpdatePatch(row, []string{"Intel Score", "ICP Band"})
	if err != nil {
		t.Fatalf("scoped null did not clear an obsolete field: %v", err)
	}
	if value, found := patch.Get("ICP Band"); !found || value != nil {
		t.Error("scoped null was not retained in update patch")
	}
	row.Set("Owner Notes", "Erase")
	if _, err := ingestUpdatePatch(row, []string{"Intel Score"}); err == nil {
		t.Error("out-of-scope field entered update patch")
	}
}

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
