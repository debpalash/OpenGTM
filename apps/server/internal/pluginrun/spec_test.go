package pluginrun

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/server/internal/contract"
)

// validateAgainstSpec checks one served response against the committed OpenAPI
// document (packages/contracts/openapi.v2.yaml), which the typed web client is
// generated from. The integration harness calls it for every request, so any
// handler change that no longer matches the spec fails the existing tests.
func validateAgainstSpec(t *testing.T, mux *http.ServeMux, req *http.Request, rec *httptest.ResponseRecorder) {
	t.Helper()
	_, pattern := mux.Handler(req)
	method, path := contract.PathOf(pattern)
	if method == "" {
		t.Errorf("%s %s matched no registered route", req.Method, req.URL.Path)
		return
	}
	if err := contract.Load(t).Validate(method, path, rec.Code, rec.Header(), rec.Body.Bytes()); err != nil {
		t.Errorf("response drifted from packages/contracts/openapi.v2.yaml: %v", err)
	}
}

// Without a database or authorization client every route answers 502 through
// the same path; that still proves each documented operation of this package
// is mounted with the documented method and path, and that the unavailable
// response is documented for it.
func TestMountedRoutesAreDocumentedAndDocumentedRoutesAreMounted(t *testing.T) {
	spec := contract.Load(t)
	mux := http.NewServeMux()
	NewAPI(nil, nil, nil).Mount(mux)

	const runID = "3f2b8c1e-9d4a-4c55-8a51-0d9b6f7a1e22"
	for _, op := range spec.Operations() {
		if op.Path == "/api/v2/version" || op.Path == "/api/v2/events" {
			continue // owned by package server; see its spec test
		}
		req := httptest.NewRequest(op.Method, strings.ReplaceAll(op.Path, "{id}", runID), nil)
		_, pattern := mux.Handler(req)
		if m, p := contract.PathOf(pattern); m != op.Method || p != op.Path {
			t.Errorf("documented %s is not mounted (matched %q)", op, pattern)
			continue
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadGateway {
			t.Errorf("%s: %d, want 502 without a database", op, rec.Code)
		}
		if err := spec.Validate(op.Method, op.Path, rec.Code, rec.Header(), rec.Body.Bytes()); err != nil {
			t.Errorf("%v", err)
		}
	}
}
