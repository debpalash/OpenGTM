package pyworker

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestCallAgainstPythonEndpoint runs against a live Python API
// (POST /internal/provider-call). Skips unless TEST_PY_WORKER_URL is set;
// TEST_PY_WORKER_TOKEN carries the INTERNAL_SERVICE_TOKEN and
// TEST_PY_WORKER_PROVIDER optionally names a provider (default: unknown, 404).
func TestCallAgainstPythonEndpoint(t *testing.T) {
	url := os.Getenv("TEST_PY_WORKER_URL")
	if url == "" {
		t.Skip("TEST_PY_WORKER_URL not set")
	}
	c := New(url, os.Getenv("TEST_PY_WORKER_TOKEN"), nil)
	prov := os.Getenv("TEST_PY_WORKER_PROVIDER")
	if prov == "" {
		prov = "no-such-provider-xyz"
	}
	res, err := c.Call(context.Background(), ProviderCall{CallID: "it-" + time.Now().Format("150405.000"), Provider: prov, WorkspaceID: "ws-it", Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == "" {
		t.Fatalf("empty status: %+v", res)
	}
	if prov == "no-such-provider-xyz" && res.Status != "error" {
		t.Fatalf("unknown provider should be error, got %+v", res)
	}
}
