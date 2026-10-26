package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/copilotauth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
)

// preFake is the Python worker's /internal/copilot/preroute endpoint.
type preFake struct {
	srv    *httptest.Server
	mu     sync.Mutex
	lines  []string
	bodies []map[string]any
	hold   bool // keep the stream open after the lines until the client goes away
	gone   chan struct{}
	opened chan struct{}
}

func newPreFake(t *testing.T, lines ...string) *preFake {
	f := &preFake{lines: lines, gone: make(chan struct{}, 4), opened: make(chan struct{}, 4)}
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/copilot/preroute", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, l := range f.lines {
			_, _ = io.WriteString(w, l+"\n")
			w.(http.Flusher).Flush()
		}
		f.opened <- struct{}{}
		if f.hold {
			<-r.Context().Done()
			f.gone <- struct{}{}
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *preFake) client() *pyworker.Client { return pyworker.New(f.srv.URL, "tok-secret-9f3", nil) }
func (f *preFake) body(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[i]
}

func preTurn(f *preFake) *Turn {
	d := Deps{PY: f.client(), Settings: settingsStub{}, TurnTimeout: 30 * time.Second}
	return &Turn{Deps: d, WS: "ws-t15", Slug: "alpha", UID: uidp(7), ConvID: "conv-t15",
		Auth: copilotauth.Resolved{WorkspaceID: "ws-t15", Slug: "alpha", UserID: uidp(7)}}
}

// recordedLines turns a recorded Python SSE body into the NDJSON frames the worker would send.
func recordedLines(t *testing.T, name string) (lines []string, body string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "contract", "copilot", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Response struct {
			Body string `json:"body"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	body = rec.Response.Body
	seq := 0
	for _, p := range strings.Split(body, "\n\n") {
		switch {
		case p == "":
		case p == "data: [DONE]":
			lines = append(lines, `{"t":"done"}`)
		case strings.HasPrefix(p, "data: "):
			seq++
			lines = append(lines, fmt.Sprintf(`{"t":"sse","seq":%d,"d":%s}`, seq, strings.TrimPrefix(p, "data: ")))
		default:
			t.Fatalf("unexpected recorded part %q", p)
		}
	}
	return lines, body
}

func runPre(t *testing.T, tn *Turn, msg string, approved bool) (handled bool, out string, err error) {
	t.Helper()
	rec := newRec()
	w, werr := ssestream.New(rec, nil)
	if werr != nil {
		t.Fatal(werr)
	}
	handled, err = tn.PreRoute(context.Background(), w, msg, approved)
	return handled, rec.String(), err
}

func TestPreRouteNoneFallsThrough(t *testing.T) {
	f := newPreFake(t, `{"t":"keepalive"}`, `{"t":"none"}`)
	handled, out, err := runPre(t, preTurn(f), "hello there", true)
	if err != nil || handled || out != "" {
		t.Fatalf("handled=%v out=%q err=%v", handled, out, err)
	}
	b := f.body(0)
	want := map[string]any{"workspace_id": "ws-t15", "user_id": float64(7), "slug": "alpha",
		"conversation_id": "conv-t15", "last_user_message": "hello there", "has_approved_calls": true}
	for k, v := range want {
		if b[k] != v {
			t.Errorf("body[%s]=%v want %v", k, b[k], v)
		}
	}
}

func TestCompanyTeamClarificationBytes(t *testing.T) {
	lines, want := recordedLines(t, "pre_company_team")
	f := newPreFake(t, lines...)
	handled, out, err := runPre(t, preTurn(f), "Stripe partnership teams", false)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if out != want {
		t.Fatalf("bytes differ\n got: %q\nwant: %q", out, want)
	}
}

func TestKeepaliveFramesAreDroppedAndGoOwnsTheTimer(t *testing.T) {
	f := newPreFake(t, `{"t":"keepalive"}`, `{"t":"keepalive"}`, `{"t":"sse","seq":1,"d":{"content":"x"}}`, `{"t":"done"}`)
	tn := preTurn(f)
	handled, out, err := runPre(t, tn, "m", false)
	if err != nil || !handled || out != "data: {\"content\": \"x\"}\n\ndata: [DONE]\n\n" {
		t.Fatalf("handled=%v out=%q err=%v", handled, out, err)
	}

	f = newPreFake(t, `{"t":"sse","seq":1,"d":{"content":"y"}}`, `{"t":"done"}`)
	f.hold = true // open and silent after the first frame; the Go ticker must keep the client alive
	tn = preTurn(f)
	tick := make(chan time.Time)
	tn.NewTicker = func(time.Duration) (<-chan time.Time, func()) { return tick, func() {} }
	rec := newRec()
	w, _ := ssestream.New(rec, nil)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = tn.PreRoute(context.Background(), w, "m", false) }()
	<-f.opened
	tick <- time.Now()
	deadline := time.After(3 * time.Second)
	for !strings.Contains(rec.String(), ": keepalive\n\n") {
		select {
		case <-deadline:
			t.Fatalf("no keepalive written: %q", rec.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	<-done
}

func TestPreRouteErrorFrameEndsStreamWithErrorEvent(t *testing.T) {
	f := newPreFake(t, `{"t":"sse","seq":1,"d":{"content":"partial"}}`, `{"t":"error","message":"preroute failed sk-SECRET-123"}`)
	handled, out, err := runPre(t, preTurn(f), "m", false)
	if !handled || err == nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	want := "data: {\"content\": \"partial\"}\n\ndata: {\"error\": \"" + errTextInterrupted + "\"}\n\ndata: [DONE]\n\n"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
	if strings.Contains(out, "SECRET") || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("worker error text leaked")
	}
}

func TestPreRouteTruncatedStreamEndsWithErrorEvent(t *testing.T) {
	f := newPreFake(t, `{"t":"sse","seq":1,"d":{"content":"partial"}}`)
	handled, out, err := runPre(t, preTurn(f), "m", false)
	if !handled || err == nil || !strings.HasSuffix(out, "data: {\"error\": \""+errTextInterrupted+"\"}\n\ndata: [DONE]\n\n") {
		t.Fatalf("handled=%v out=%q err=%v", handled, out, err)
	}
}

func TestPreRouteNoneAfterOutputIsHandledWithoutDone(t *testing.T) {
	// Python's router ignores a late none and simply ends the response: nothing more is written.
	f := newPreFake(t, `{"t":"sse","seq":1,"d":{"content":"a"}}`, `{"t":"none"}`)
	handled, out, err := runPre(t, preTurn(f), "m", false)
	if err != nil || !handled || out != "data: {\"content\": \"a\"}\n\n" {
		t.Fatalf("handled=%v out=%q err=%v", handled, out, err)
	}
}

func TestPreRouteWithoutToolUserFailsClosed(t *testing.T) {
	f := newPreFake(t, `{"t":"none"}`)
	tn := preTurn(f)
	tn.Auth = copilotauth.Resolved{WorkspaceID: "ws-t15"} // cloud shape without a user
	handled, out, err := runPre(t, tn, "m", false)
	if !handled || err == nil || !strings.Contains(out, `{"error": "`+errTextTools+`"}`) {
		t.Fatalf("handled=%v out=%q err=%v", handled, out, err)
	}
	f.mu.Lock()
	n := len(f.bodies)
	f.mu.Unlock()
	if n != 0 {
		t.Fatal("worker was called without a user")
	}
}

func TestPreRouteDisconnectCancels(t *testing.T) {
	f := newPreFake(t, `{"t":"sse","seq":1,"d":{"content":"a"}}`)
	f.hold = true
	tn := preTurn(f)
	ctx, cancel := context.WithCancel(context.Background())
	rec := newRec()
	w, _ := ssestream.New(rec, nil)
	errc := make(chan error, 1)
	go func() { _, err := tn.PreRoute(ctx, w, "m", false); errc <- err }()
	<-f.opened
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("want a context error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("PreRoute did not return after cancel")
	}
	select {
	case <-f.gone:
	case <-time.After(3 * time.Second):
		t.Fatal("worker request was not cancelled")
	}
	if strings.Contains(rec.String(), "error") || strings.Contains(rec.String(), "DONE") {
		t.Fatalf("frames written after disconnect: %q", rec.String())
	}
}

func TestPreRouteBadSSEFrameIsProtocolError(t *testing.T) {
	f := newPreFake(t, `{"t":"sse","seq":1}`, `{"t":"done"}`)
	handled, out, err := runPre(t, preTurn(f), "m", false)
	if !handled || err == nil || !strings.Contains(out, errTextInterrupted) {
		t.Fatalf("handled=%v out=%q err=%v", handled, out, err)
	}
}
