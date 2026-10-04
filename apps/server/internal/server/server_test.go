package server

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/authz"
	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"github.com/debpalash/OpenGTM/apps/server/internal/progress"
)

var quiet = slog.New(slog.DiscardHandler)

// legacy is a stand-in FastAPI recording what the proxy forwards.
type legacy struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (l *legacy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	l.reqs = append(l.reqs, r.Clone(context.Background()))
	l.mu.Unlock()
	switch {
	case r.URL.Path == "/health":
		writeJSON(w, 200, map[string]string{"status": "healthy"})
	case r.URL.Path == authz.ContextPath:
		if r.Header.Get("Authorization") != "Bearer good" {
			writeJSON(w, 401, map[string]string{"detail": "Could not validate credentials"})
			return
		}
		ws := r.Header.Get("X-Workspace-Id")
		if ws == "" {
			ws = "ws-1"
		}
		writeJSON(w, 200, authz.Workspace{UserID: "1", Username: "u", WorkspaceID: ws, Slug: ws, Role: "owner"})
	case r.URL.Path == "/api/stream":
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // never finishes on its own
	case r.Header.Get("Upgrade") == "websocket":
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(rw, buf); err == nil {
			rw.Write(buf)
			rw.Flush()
		}
	default:
		w.Header().Set("X-Frame-Options", "DENY") // must not duplicate ours
		writeJSON(w, 200, map[string]string{"path": r.URL.Path, "method": r.Method})
	}
}

func (l *legacy) last() *http.Request {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reqs[len(l.reqs)-1]
}

type fixture struct {
	srv    *httptest.Server
	legacy *legacy
	hub    *progress.Hub
}

func newFixture(t *testing.T, web fstest.MapFS, closing chan struct{}) *fixture {
	t.Helper()
	l := &legacy{}
	up := httptest.NewServer(l)
	t.Cleanup(up.Close)
	az, err := authz.New(up.URL, authz.Options{})
	if err != nil {
		t.Fatal(err)
	}
	hub := progress.NewHub(nil, quiet)
	if web == nil {
		web = fstest.MapFS{}
	}
	h, err := New(Deps{
		Config:    config.Config{LegacyAPIURL: up.URL},
		Authz:     az,
		Hub:       hub,
		Logger:    quiet,
		Version:   "v-test",
		Web:       web,
		Closing:   closing,
		Heartbeat: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &fixture{srv: srv, legacy: l, hub: hub}
}

func get(t *testing.T, url string, header map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultTransport.RoundTrip(req) // no transparent gzip
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func body(t *testing.T, r *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestHealthVersionAndReadiness(t *testing.T) {
	f := newFixture(t, nil, nil)
	if r := get(t, f.srv.URL+"/healthz", nil); r.StatusCode != 200 {
		t.Fatalf("/healthz = %d", r.StatusCode)
	}
	r := get(t, f.srv.URL+"/api/v2/version", nil)
	var v map[string]string
	json.NewDecoder(r.Body).Decode(&v)
	if v["version"] != "v-test" || !strings.HasPrefix(v["go"], "go") {
		t.Fatalf("version = %v", v)
	}
	// No database configured: not ready, but the legacy API is reported.
	r = get(t, f.srv.URL+"/readyz", nil)
	var ready struct {
		Status string                    `json:"status"`
		Checks map[string]map[string]any `json:"checks"`
	}
	json.NewDecoder(r.Body).Decode(&ready)
	if r.StatusCode != 503 || ready.Status != "not_ready" || ready.Checks["legacy_api"]["ok"] != true {
		t.Fatalf("readyz = %d %+v", r.StatusCode, ready)
	}
	if r.Header.Get("X-Frame-Options") != "SAMEORIGIN" || r.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security headers missing: %v", r.Header)
	}
}

func TestLegacyRoutesAreProxiedLikeNginx(t *testing.T) {
	f := newFixture(t, fstest.MapFS{"index.html": {Data: []byte("<html>spa</html>")}}, nil)
	for _, p := range []string{"/api/leads?x=1", "/api", "/auth/token", "/health", "/scim/v2/acme/Users",
		"/admin/users", "/ws/scraper/preview", "/mcp", "/api/v2/workbooks/abc", "/api/events"} {
		r := get(t, f.srv.URL+p, map[string]string{"X-Forwarded-For": "203.0.113.9", "X-Request-Id": "req-123"})
		if r.StatusCode != 200 || strings.Contains(body(t, r), "spa") {
			t.Fatalf("%s was not proxied (status %d)", p, r.StatusCode)
		}
		up := f.legacy.last()
		wantPath, _, _ := strings.Cut(p, "?")
		if up.URL.Path != wantPath || up.URL.RequestURI() != p {
			t.Fatalf("%s forwarded as %s", p, up.URL.RequestURI())
		}
		if up.Host != strings.TrimPrefix(f.srv.URL, "http://") {
			t.Fatalf("Host not preserved: %q", up.Host)
		}
		if got := up.Header.Get("X-Forwarded-For"); got != "203.0.113.9, 127.0.0.1" {
			t.Fatalf("X-Forwarded-For = %q", got)
		}
		if up.Header.Get("X-Real-IP") != "127.0.0.1" || up.Header.Get("X-Forwarded-Proto") != "http" ||
			up.Header.Get("X-Request-Id") != "req-123" {
			t.Fatalf("forwarding headers: %v", up.Header)
		}
		if vals := r.Header.Values("X-Frame-Options"); len(vals) != 1 || vals[0] != "SAMEORIGIN" {
			t.Fatalf("X-Frame-Options = %v", vals)
		}
		if r.Header.Get("X-Request-Id") != "req-123" {
			t.Fatal("request id not echoed")
		}
	}
	// Methods other than GET on Go-owned paths still reach FastAPI.
	resp, err := http.Post(f.srv.URL+"/api/v2/events", "application/json", nil)
	if err != nil || resp.StatusCode != 200 || f.legacy.last().Method != "POST" {
		t.Fatalf("POST /api/v2/events not proxied: %v %v", err, resp.Status)
	}
	resp.Body.Close()
}

func TestProxyStreamsAndUpgrades(t *testing.T) {
	f := newFixture(t, nil, nil)
	// SSE: the first event must arrive while the upstream is still open.
	r := get(t, f.srv.URL+"/api/stream", nil)
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(r.Body).ReadString('\n')
		line <- s
	}()
	select {
	case s := <-line:
		if s != "data: first\n" {
			t.Fatalf("stream line %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxied SSE was buffered")
	}

	// WebSocket upgrade with a raw echo after the handshake.
	conn, err := net.Dial("tcp", strings.TrimPrefix(f.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(conn, "GET /api/workbooks/w1/ws?token=t HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\n"+
		"Connection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", err, resp)
	}
	io.WriteString(conn, "ping")
	echo := make([]byte, 4)
	if _, err := io.ReadFull(br, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("echo %q %v", echo, err)
	}
}

func TestLegacyDownIs502(t *testing.T) {
	h, err := New(Deps{Config: config.Config{LegacyAPIURL: "http://127.0.0.1:1"}, Logger: quiet, Web: fstest.MapFS{}})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/leads", nil))
	if w.Code != 502 || !strings.Contains(w.Body.String(), "Legacy API unavailable") {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
}

func TestSPAServing(t *testing.T) {
	js := strings.Repeat("console.log('opengtm');\n", 200)
	f := newFixture(t, fstest.MapFS{
		"index.html":          {Data: []byte("<html>spa</html>")},
		"assets/app-1a2b3.js": {Data: []byte(js)},
		"favicon.ico":         {Data: []byte("ico")},
		".keep":               {Data: nil},
	}, nil)

	for _, p := range []string{"/", "/index.html", "/leads/42", "/workbooks/abc/settings", "/.keep"} {
		r := get(t, f.srv.URL+p, nil)
		if r.StatusCode != 200 || body(t, r) != "<html>spa</html>" || r.Header.Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s: %d cache=%q", p, r.StatusCode, r.Header.Get("Cache-Control"))
		}
	}
	r := get(t, f.srv.URL+"/assets/app-1a2b3.js", map[string]string{"Accept-Encoding": "gzip"})
	if r.StatusCode != 200 || r.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" ||
		r.Header.Get("Content-Encoding") != "gzip" || !strings.Contains(r.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("asset headers: %d %v", r.StatusCode, r.Header)
	}
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if plain, _ := io.ReadAll(zr); string(plain) != js {
		t.Fatal("gzip body mismatch")
	}
	r = get(t, f.srv.URL+"/assets/app-1a2b3.js", nil)
	if r.Header.Get("Content-Encoding") != "" || body(t, r) != js {
		t.Fatal("identity encoding expected without Accept-Encoding")
	}
	if r := get(t, f.srv.URL+"/assets/stale-999.js", nil); r.StatusCode != 404 {
		t.Fatalf("missing asset = %d, want 404", r.StatusCode)
	}
	if r := get(t, f.srv.URL+"/favicon.ico", nil); r.Header.Get("Cache-Control") != "public, max-age=3600" {
		t.Fatalf("unhashed file cache = %q", r.Header.Get("Cache-Control"))
	}
	resp, _ := http.Post(f.srv.URL+"/leads", "text/plain", nil)
	if resp.StatusCode != 405 {
		t.Fatalf("POST to SPA = %d", resp.StatusCode)
	}
	resp.Body.Close()

	empty := newFixture(t, fstest.MapFS{}, nil)
	if r := get(t, empty.srv.URL+"/", nil); r.StatusCode != 404 || !strings.Contains(body(t, r), "OPENGTM_WEB_DIR") {
		t.Fatalf("unbuilt UI: %d", r.StatusCode)
	}
}

func TestEventsStreamIsAuthorizedAndWorkspaceScoped(t *testing.T) {
	closing := make(chan struct{})
	f := newFixture(t, nil, closing)

	if r := get(t, f.srv.URL+"/api/v2/events", nil); r.StatusCode != 401 {
		t.Fatalf("anonymous events = %d", r.StatusCode)
	}
	if r := get(t, f.srv.URL+"/api/v2/events?token=bad", nil); r.StatusCode != 401 {
		t.Fatalf("bad token events = %d", r.StatusCode)
	}

	r := get(t, f.srv.URL+"/api/v2/events?token=good&workspace_id=ws-7", nil)
	if r.StatusCode != 200 || r.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events = %d %v", r.StatusCode, r.Header)
	}
	if up := f.legacy.last(); up.Header.Get("X-Workspace-Id") != "ws-7" {
		t.Fatalf("workspace not forwarded to authz: %v", up.Header)
	}
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(r.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	waitFor := func(pred func(string) bool) string {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatal("stream closed")
				}
				if pred(l) {
					return l
				}
			case <-deadline:
				t.Fatal("timed out waiting for stream line")
			}
		}
	}
	waitFor(func(l string) bool { return l == ": connected" })
	waitFor(func(l string) bool { return l == ": heartbeat" })

	f.hub.Dispatch(progress.Event{WorkspaceID: "ws-other", Event: "leak"})
	f.hub.Dispatch(progress.Event{WorkspaceID: "ws-7", Event: "mine", Data: json.RawMessage(`{"n":1}`)})
	l := waitFor(func(l string) bool { return strings.HasPrefix(l, "data: ") })
	var e progress.Event
	json.Unmarshal([]byte(strings.TrimPrefix(l, "data: ")), &e)
	if e.Event != "mine" || e.WorkspaceID != "ws-7" {
		t.Fatalf("received %+v; another workspace's event leaked or ours was lost", e)
	}

	close(closing) // server shutdown ends the stream
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-lines:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("stream did not end on shutdown")
		}
	}
}
