package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestJobLoggerAttrs(t *testing.T) {
	var buf bytes.Buffer
	JobLogger(NewLogger(&buf), 42, "ws-1", 7).Info("hi")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["job_id"] != float64(42) || m["workspace_id"] != "ws-1" || m["lease_token"] != float64(7) {
		t.Fatalf("attrs missing: %v", m)
	}
}

func TestRejectsNonLoopback(t *testing.T) {
	for _, a := range []string{"0.0.0.0:6060", ":6060", "localhost:6060", "10.0.0.1:6060", "[::]:6060", "nohost"} {
		if _, err := StartDebugServer(context.Background(), a); err == nil {
			t.Errorf("%q accepted", a)
		}
	}
}

func TestEndpoints(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := StartDebugServer(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"/metrics": "go_goroutines", "/debug/pprof/": "goroutine"} {
		resp, err := http.Get("http://" + srv.Addr + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body), want) {
			t.Errorf("%s: status %d body %.80q", path, resp.StatusCode, body)
		}
		if path == "/metrics" && (!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") || !strings.Contains(string(body), "process_uptime_seconds")) {
			t.Errorf("metrics format: %q", body)
		}
	}
}
