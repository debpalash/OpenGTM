package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeResolve authenticates "good-<ws>" tokens into workspace <ws>; anything
// else is an AuthError. It never touches a database.
func fakeResolve(ctx context.Context, d Deps, raw string) (Ctx, error) {
	if raw == "" {
		return Ctx{}, &AuthError{Msg: "missing MCP token"}
	}
	if ws, ok := strings.CutPrefix(raw, "good-"); ok {
		return Ctx{WorkspaceID: ws, Slug: ws, Capabilities: keylessCaps()}, nil
	}
	return Ctx{}, &AuthError{Msg: "invalid MCP token"}
}

func sseServer(t *testing.T) (*sseHandler, *Server) {
	t.Helper()
	old := resolveToken
	resolveToken = fakeResolve
	t.Cleanup(func() { resolveToken = old })
	d := Deps{Cfg: Config{RequireAuth: true}}
	s := NewServer(d, NewRegistry(nil, nil))
	return SSEHandler(s, d).(*sseHandler), s
}

func post(h http.Handler, auth, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://127.0.0.1:3100/message", strings.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestSSEEndpointEventBytesMatchFixture(t *testing.T) {
	h, _ := sseServer(t)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:3100/sse", nil))
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	if w.Body.String() != "data: {\"type\": \"endpoint\", \"url\": \"/message\"}\n\n" {
		t.Fatalf("body %q", w.Body.String())
	}
	// Only /sse is unauthenticated and nothing else is routed.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "http://127.0.0.1:3100/other", strings.NewReader("{}")))
	if w.Code != 404 || strings.TrimSpace(w.Body.String()) != `{"detail":"Not Found"}` {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
}

func TestMessageUnauthorizedMatchesFixtures(t *testing.T) {
	h, _ := sseServer(t)
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	for _, c := range []struct{ auth, detail string }{
		{"", "missing MCP token"},
		{"Basic abc", "missing MCP token"},
		{"Bearer ", "missing MCP token"},
		{"Bearer nope", "invalid MCP token"},
	} {
		w := post(h, c.auth, ping)
		if w.Code != 401 || w.Header().Get("WWW-Authenticate") != "Bearer" || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%q: %d %v", c.auth, w.Code, w.Header())
		}
		var got map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if got["error"] != "unauthorized" || got["detail"] != c.detail || len(got) != 2 {
			t.Fatalf("%q: %s", c.auth, w.Body.String())
		}
	}
}

type countingBody struct{ n atomic.Int64 }

func (c *countingBody) Read(p []byte) (int, error) {
	c.n.Add(1)
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

func TestMessageAuthBeforeBodyRead(t *testing.T) {
	h, _ := sseServer(t)
	body := &countingBody{}
	r := httptest.NewRequest("POST", "http://127.0.0.1:3100/message", io.NopCloser(body))
	r.Header.Set("Authorization", "Bearer bad")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 || body.n.Load() != 0 {
		t.Fatalf("status %d, body reads %d", w.Code, body.n.Load())
	}
}

func TestMessageInitializeListCallMatchFixtures(t *testing.T) {
	h, _ := sseServer(t)
	w := post(h, "Bearer good-a", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || !strings.Contains(w.Body.String(), `"protocolVersion"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// Lower-case scheme accepted (fixture read__initialize_lowercase_scheme).
	if w = post(h, "bearer good-a", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`); !strings.Contains(w.Body.String(), `"find_leads"`) {
		t.Fatalf("%s", w.Body.String())
	}
	w = post(h, "Bearer good-a", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"isError":true`) && !strings.Contains(w.Body.String(), `"isError": true`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestMessageNotificationReturnsNull(t *testing.T) {
	h, _ := sseServer(t)
	w := post(h, "Bearer good-a", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if w.Code != 200 || w.Body.String() != "null" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
}

func TestMessageInvalidJSONIs200ParseError(t *testing.T) {
	h, _ := sseServer(t)
	w := post(h, "Bearer good-a", `{not json`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "-32700") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestMessageBodyCap413(t *testing.T) {
	h, _ := sseServer(t)
	w := post(h, "Bearer good-a", strings.Repeat(" ", maxMessageBody+1))
	if w.Code != 413 {
		t.Fatalf("%d", w.Code)
	}
	// Exactly at the cap is read (the protocol layer answers).
	if w = post(h, "Bearer good-a", strings.Repeat(" ", maxMessageBody)); w.Code != 200 {
		t.Fatalf("at cap: %d", w.Code)
	}
}

func TestMessageCrossTenantTokenIsolation(t *testing.T) {
	h, s := sseServer(t)
	var seen atomic.Value
	s.Execute = func(_ context.Context, c Ctx, name string, _ Args) string {
		seen.Store(c.WorkspaceID)
		return "{}"
	}
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"find_leads","arguments":{"workspace_id":"ws-b"}}}`
	for _, ws := range []string{"a", "b"} {
		post(h, "Bearer good-"+ws, call)
		if seen.Load() != ws {
			t.Fatalf("token %s ran in %v", ws, seen.Load())
		}
	}
}

func TestListenRefusesNonLoopback(t *testing.T) {
	t.Setenv("OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK", "")
	h, _ := sseServer(t)
	for _, host := range []string{"0.0.0.0", "10.1.2.3", "example.com", "::"} {
		if err := ListenSSE(context.Background(), h, host, 0); err == nil || !strings.Contains(err.Error(), "non-loopback") {
			t.Fatalf("%s: %v", host, err)
		}
	}
}

func TestListenAllowsOverride(t *testing.T) {
	t.Setenv("OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK", "true")
	h, _ := sseServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- ListenSSE(ctx, h, "0.0.0.0", 0) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not shut down")
	}
}

func TestListenLoopbackServesAndShutsDown(t *testing.T) {
	h, _ := sseServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- serveSSE(ctx, h, ln) }()
	resp, err := http.Get("http://" + ln.Addr().String() + "/sse")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(b, []byte("endpoint")) {
		t.Fatalf("%q", b)
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestServerLimitsMatchSpec(t *testing.T) {
	srv := newHTTPServer(http.NotFoundHandler(), 120*time.Second)
	if srv.ReadHeaderTimeout != 10*time.Second || srv.ReadTimeout != 30*time.Second || srv.WriteTimeout != 130*time.Second ||
		srv.IdleTimeout != 60*time.Second || srv.MaxHeaderBytes != 16<<10 {
		t.Fatalf("%+v", srv)
	}
}

func TestFailedAuthRateLimit429(t *testing.T) {
	h, _ := sseServer(t)
	send := func(tok string) int {
		r := httptest.NewRequest("POST", "http://127.0.0.1:3100/message", strings.NewReader("{}"))
		r.RemoteAddr = "127.0.0.1:5000"
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < 30; i++ {
		// Rotating tokens must not evade the per-IP limit.
		if c := send("bad" + string(rune('a'+i%26)) + string(rune('a'+i/26))); c != 401 {
			t.Fatalf("attempt %d: %d", i, c)
		}
	}
	if c := send("bad-more"); c != 429 {
		t.Fatalf("31st failure: %d", c)
	}
}

func TestOverLimitIPSkipsTokenLookup(t *testing.T) {
	t.Setenv("TRUST_PROXY", "true")
	h, _ := sseServer(t)
	var lookups atomic.Int64
	resolveToken = func(ctx context.Context, d Deps, raw string) (Ctx, error) {
		lookups.Add(1)
		return fakeResolve(ctx, d, raw)
	}
	send := func(tok, xff string) int {
		r := httptest.NewRequest("POST", "http://127.0.0.1:3100/message", strings.NewReader("{}"))
		r.RemoteAddr = "127.0.0.1:5000"
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < 30; i++ {
		// A spoofed X-Forwarded-For must not rotate the key, even with TRUST_PROXY.
		send("bad", "203.0.113."+string(rune('1'+i%9)))
	}
	before := lookups.Load()
	if c := send("good-a", "198.51.100.9"); c != 429 {
		t.Fatalf("over-limit valid token: %d", c)
	}
	if lookups.Load() != before {
		t.Fatal("over-limit request reached the token lookup")
	}
	// Another peer is unaffected.
	r := httptest.NewRequest("POST", "http://127.0.0.1:3100/message", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	r.RemoteAddr = "127.0.0.2:5000"
	r.Header.Set("Authorization", "Bearer good-a")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("other peer: %d", w.Code)
	}
}

func TestHostAndOriginMustBeLoopback(t *testing.T) {
	t.Setenv("OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK", "")
	h, _ := sseServer(t)
	do := func(method, target, host, origin string) int {
		r := httptest.NewRequest(method, "http://127.0.0.1:3100"+target, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		if host != "" {
			r.Host = host
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		r.Header.Set("Authorization", "Bearer good-a")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, c := range []struct {
		method, target, host, origin string
		want                         int
	}{
		{"GET", "/sse", "evil.example.com", "", 403},
		{"POST", "/message", "evil.example.com:3100", "", 403},
		{"POST", "/message", "attacker.test", "", 403},
		{"POST", "/message", "127.0.0.1:3100", "https://evil.example.com", 403},
		{"POST", "/message", "localhost:3100", "http://evil.example.com:3100", 403},
		{"POST", "/message", "127.0.0.1:3100", "null", 403},
		{"GET", "/sse", "127.0.0.1:3100", "", 200},
		{"GET", "/sse", "localhost:3100", "", 200},
		{"GET", "/sse", "[::1]:3100", "", 200},
		{"POST", "/message", "localhost", "http://localhost:3000", 200},
	} {
		if got := do(c.method, c.target, c.host, c.origin); got != c.want {
			t.Errorf("%s %s host=%q origin=%q: %d want %d", c.method, c.target, c.host, c.origin, got, c.want)
		}
	}
	t.Setenv("OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK", "true")
	if got := do("GET", "/sse", "gtm.internal:3100", ""); got != 200 {
		t.Errorf("override: %d", got)
	}
}

func TestConcurrentRequestCap503(t *testing.T) {
	h, s := sseServer(t)
	block := make(chan struct{})
	entered := make(chan struct{}, maxConcurrentRequests)
	s.Execute = func(ctx context.Context, _ Ctx, _ string, _ Args) string {
		entered <- struct{}{}
		<-block
		return "{}"
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	defer close(block)
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"find_leads"}}`
	for i := 0; i < maxConcurrentRequests; i++ {
		go func() {
			req, _ := http.NewRequest("POST", srv.URL+"/message", strings.NewReader(call))
			req.Header.Set("Authorization", "Bearer good-a")
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}()
	}
	for i := 0; i < maxConcurrentRequests; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("requests did not reach the handler")
		}
	}
	resp, err := http.Get(srv.URL + "/sse")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestSlowClientTimesOut(t *testing.T) {
	h, _ := sseServer(t)
	srv := newHTTPServer(h, time.Second)
	srv.ReadHeaderTimeout = 200 * time.Millisecond
	srv.ReadTimeout = 300 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()

	// Slowloris on the header: never finishes the request line.
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("POST /message HTTP/1.1\r\nHost: x\r\n"))
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(c); err != nil && os.IsTimeout(err) {
		t.Fatal("server did not close a stalled header")
	}

	// Slow body: valid auth, promised body never arrives.
	c2, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	_, _ = c2.Write([]byte("POST /message HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer good-a\r\nContent-Length: 100\r\n\r\n{"))
	_ = c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(c2); err != nil && os.IsTimeout(err) {
		t.Fatal("server did not time out a stalled body")
	}
}
