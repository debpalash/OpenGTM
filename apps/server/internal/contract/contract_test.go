package contract

import (
	"net/http"
	"strings"
	"testing"
)

func TestSpecLoadsAndIsWellFormed(t *testing.T) {
	spec := Load(t)
	ops := spec.Operations()
	if len(ops) < 8 {
		t.Fatalf("only %d operations: %v", len(ops), ops)
	}
	seen := map[string]Operation{}
	for _, op := range ops {
		if op.ID == "" {
			t.Errorf("%s has no operationId (the generated client names functions after it)", op)
		}
		if prev, dup := seen[op.ID]; dup {
			t.Errorf("operationId %q used by %s and %s", op.ID, prev, op)
		}
		seen[op.ID] = op
		if !strings.HasPrefix(op.Path, "/api/v2/") {
			t.Errorf("%s is outside /api/v2", op)
		}
	}
}

func okHeader() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return h
}

const goodRun = `{"id":"3f2b8c1e-9d4a-4c55-8a51-0d9b6f7a1e22","plugin":"acme","plugin_version":"1.0.0","status":"pending",
 "inputs":{"domain":"acme.example"},"job_id":7,"created_by":"ana","created_at":"2026-10-05T10:00:00.123456Z",
 "started_at":null,"completed_at":null,"error":null,"stats":{}}`

func TestValidateAcceptsAndRejects(t *testing.T) {
	spec := Load(t)
	h := okHeader()

	page := `{"runs":[` + goodRun + `],"next_cursor":null}`
	if err := spec.Validate("GET", "/api/v2/plugin-runs", 200, h, []byte(page)); err != nil {
		t.Fatalf("valid page rejected: %v", err)
	}
	cases := map[string]struct {
		status int
		body   string
		want   string
	}{
		"missing field":     {200, `{"runs":[]}`, "next_cursor"},
		"wrong type":        {200, `{"runs":[],"next_cursor":5}`, "next_cursor"},
		"incomplete run":    {200, `{"runs":[{"id":"x"}],"next_cursor":null}`, "plugin"},
		"bad enum":          {200, `{"runs":[` + strings.Replace(goodRun, `"pending"`, `"weird"`, 1) + `],"next_cursor":null}`, "status"},
		"bad uuid":          {200, `{"runs":[` + strings.Replace(goodRun, "3f2b8c1e-9d4a-4c55-8a51-0d9b6f7a1e22", "nope", 1) + `],"next_cursor":null}`, "uuid"},
		"undocumented code": {418, `{"detail":"x"}`, "does not document"},
		"error shape":       {400, `{"message":"x"}`, "detail"},
	}
	for name, tc := range cases {
		err := spec.Validate("GET", "/api/v2/plugin-runs", tc.status, h, []byte(tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one mentioning %q", name, err, tc.want)
		}
	}
	if err := spec.Validate("GET", "/api/v2/plugin-runs", 400, h, []byte(`{"detail":"Invalid cursor"}`)); err != nil {
		t.Fatalf("documented error rejected: %v", err)
	}
	if err := spec.Validate("GET", "/api/v2/nope", 200, h, []byte(`{}`)); err == nil {
		t.Fatal("unknown path accepted")
	}
	// 503 must carry Retry-After.
	if err := spec.Validate("GET", "/api/v2/plugins", 503, h, []byte(`{"detail":"x"}`)); err == nil || !strings.Contains(err.Error(), "Retry-After") {
		t.Fatalf("503 without Retry-After: %v", err)
	}
	h503 := okHeader()
	h503.Set("Retry-After", "1")
	if err := spec.Validate("GET", "/api/v2/plugins", 503, h503, []byte(`{"detail":"x"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestPathOf(t *testing.T) {
	m, p := PathOf("POST /api/v2/plugin-runs/{id}/cancel")
	if m != "POST" || p != "/api/v2/plugin-runs/{id}/cancel" {
		t.Fatalf("%q %q", m, p)
	}
}
