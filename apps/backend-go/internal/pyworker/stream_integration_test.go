package pyworker

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

// TestStreamAgainstPythonEndpoint runs the preroute RPC against a live Python
// API. Skips unless TEST_PY_WORKER_URL is set (TEST_PY_WORKER_TOKEN carries the
// INTERNAL_SERVICE_TOKEN).
func TestStreamAgainstPythonEndpoint(t *testing.T) {
	url := os.Getenv("TEST_PY_WORKER_URL")
	if url == "" {
		t.Skip("TEST_PY_WORKER_URL not set")
	}
	c := New(url, os.Getenv("TEST_PY_WORKER_TOKEN"), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := c.Stream(ctx, "/internal/copilot/preroute", map[string]any{
		"workspace_id": "ws-it", "slug": "it", "last_user_message": "hello", "has_approved_calls": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for {
		f, err := s.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if f.T == "" {
			t.Fatal("frame without type")
		}
	}
}
