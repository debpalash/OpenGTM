package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/contract"
	"github.com/debpalash/OpenGTM/apps/server/internal/progress"
)

// The Go-owned routes of this package must keep matching the OpenAPI document
// the typed web client is generated from (packages/contracts/openapi.v2.yaml).
func TestVersionAndEventsMatchTheSpec(t *testing.T) {
	spec := contract.Load(t)
	closing := make(chan struct{})
	f := newFixture(t, nil, closing)

	r := get(t, f.srv.URL+"/api/v2/version", nil)
	b, _ := io.ReadAll(r.Body)
	if err := spec.Validate("GET", "/api/v2/version", r.StatusCode, r.Header, b); err != nil {
		t.Fatal(err)
	}

	// Refusals are documented too.
	r = get(t, f.srv.URL+"/api/v2/events", nil)
	b, _ = io.ReadAll(r.Body)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous events = %d", r.StatusCode)
	}
	if err := spec.Validate("GET", "/api/v2/events", r.StatusCode, r.Header, b); err != nil {
		t.Fatal(err)
	}

	// The stream: documented status and media type, and every data frame is a
	// ProgressEvent.
	r = get(t, f.srv.URL+"/api/v2/events?token=good&workspace_id=ws-7", nil)
	if err := spec.Validate("GET", "/api/v2/events", r.StatusCode, r.Header, nil); err != nil {
		t.Fatal(err)
	}
	frames := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(r.Body)
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "data: ") {
				frames <- strings.TrimPrefix(l, "data: ")
			}
		}
	}()
	at := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	for _, e := range []progress.Event{
		{WorkspaceID: "ws-7", Event: "plugin_run_queued", Data: json.RawMessage(`{"run_id":"r1","plugin":"acme"}`), At: at},
		{WorkspaceID: "ws-7", Event: "big", Truncated: true, At: at},
	} {
		f.hub.Dispatch(e)
		select {
		case frame := <-frames:
			if err := spec.ValidateSchema("ProgressEvent", []byte(frame)); err != nil {
				t.Fatalf("SSE frame %q drifted from the spec: %v", frame, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no event frame")
		}
	}
	close(closing)
}
