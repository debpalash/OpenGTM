package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const stdioSecret = "ycp_super_secret_token_value"

func stdioCtx() Ctx {
	return Ctx{WorkspaceID: "ws-alpha", Slug: "alpha", Capabilities: map[string]bool{"leads:read": true}, TokenID: "mcpt-read"}
}

// stdioEnv swaps the session resolver and the clock; both are restored on cleanup.
type stdioEnv struct {
	mu     sync.Mutex
	calls  []string
	resolv func(raw string, n int) (Ctx, error)
	stderr bytes.Buffer
}

func newStdioEnv(t *testing.T) *stdioEnv {
	t.Helper()
	e := &stdioEnv{resolv: func(string, int) (Ctx, error) { return stdioCtx(), nil }}
	oldR, oldE, oldN := resolveSession, stdioStderr, now
	resolveSession = func(_ context.Context, _ Deps, raw string) (Ctx, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.calls = append(e.calls, raw)
		return e.resolv(raw, len(e.calls))
	}
	stdioStderr = &e.stderr
	t.Cleanup(func() { resolveSession, stdioStderr, now = oldR, oldE, oldN })
	return e
}

func (e *stdioEnv) nCalls() int { e.mu.Lock(); defer e.mu.Unlock(); return len(e.calls) }

func stdioDeps() Deps { return Deps{Cfg: Config{RequireAuth: true, StdioRevalidateSeconds: 30}} }

func run(t *testing.T, s *Server, d Deps, in string) (string, error) {
	t.Helper()
	return runR(t, s, d, strings.NewReader(in))
}

func runR(t *testing.T, s *Server, d Deps, in io.Reader) (string, error) {
	t.Helper()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- ServeStdio(context.Background(), s, d, in, &out, stdioSecret) }()
	select {
	case err := <-done:
		return out.String(), err
	case <-time.After(10 * time.Second):
		t.Fatal("ServeStdio hung")
		return "", nil
	}
}

func frame(body string) string { return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body) }

// parseFrames strictly parses Content-Length frames; any other byte fails.
func parseFrames(t *testing.T, out string) []string {
	t.Helper()
	var bodies []string
	for len(out) > 0 {
		if !strings.HasPrefix(out, "Content-Length: ") {
			t.Fatalf("non-protocol bytes on stdout: %q", out)
		}
		hdr, rest, ok := strings.Cut(out, "\r\n\r\n")
		if !ok {
			t.Fatalf("bad header: %q", out)
		}
		var n int
		if _, err := fmt.Sscanf(hdr, "Content-Length: %d", &n); err != nil || n > len(rest) {
			t.Fatalf("bad frame: %q", hdr)
		}
		bodies = append(bodies, rest[:n])
		out = rest[n:]
	}
	return bodies
}

func newStdioServer() *Server { return NewServer(stdioDeps(), NewRegistry(nil, nil)) }

type stdioFixture struct {
	Token   *string `json:"token"`
	Request struct {
		Stdin string `json:"stdin"`
	} `json:"request"`
	Response struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	} `json:"response"`
}

func loadStdioFixture(t *testing.T, name string) stdioFixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureRoot, "stdio", name))
	if err != nil {
		t.Fatal(err)
	}
	var f stdioFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestStdioInitializeListCallRoundTrip(t *testing.T) {
	// Fixtures that need no database; find_leads / unicode_result replay in the integration test.
	for _, name := range []string{"read__initialize_ping_list.json", "read__capability_denied.json"} {
		e := newStdioEnv(t)
		fx := loadStdioFixture(t, name)
		got, err := run(t, newStdioServer(), stdioDeps(), fx.Request.Stdin)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := fx.Response.Stdout
		if name == "read__capability_denied.json" { // F7: tool errors set isError (Python recorded false)
			want = strings.Replace(strings.Replace(want, `"isError": false`, `"isError": true`, 1), "Content-Length: 171", "Content-Length: 170", 1)
		}
		if got != want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, want)
		}
		if e.nCalls() != 1 || e.calls[0] != stdioSecret {
			t.Errorf("%s: resolver calls %v", name, e.calls)
		}
	}
}

func TestStdioRefusesWithoutValidToken(t *testing.T) {
	for _, name := range []string{"read__no_token_exits.json", "read__invalid_token_exits.json"} {
		e := newStdioEnv(t)
		e.resolv = func(string, int) (Ctx, error) { return Ctx{}, &AuthError{Msg: "invalid MCP token"} }
		fx := loadStdioFixture(t, name)
		if fx.Response.ExitCode != 1 || fx.Response.Stdout != "" {
			t.Fatalf("fixture %s unexpected: %+v", name, fx.Response)
		}
		got, err := run(t, newStdioServer(), stdioDeps(), fx.Request.Stdin)
		if err == nil {
			t.Fatalf("%s: want non-nil error", name)
		}
		if got != "" {
			t.Errorf("%s: stdout must stay empty, got %q", name, got)
		}
		want := "MCP stdio refused: invalid MCP token. Set OPENGTM_MCP_TOKEN to a valid token."
		if !strings.Contains(e.stderr.String(), want) {
			t.Errorf("%s: stderr %q lacks %q", name, e.stderr.String(), want)
		}
		if strings.Contains(e.stderr.String(), stdioSecret) || strings.Contains(err.Error(), stdioSecret) {
			t.Errorf("%s: token leaked", name)
		}
	}
}

func TestStdioRefusalMasksNonAuthErrors(t *testing.T) {
	e := newStdioEnv(t)
	e.resolv = func(string, int) (Ctx, error) {
		return Ctx{}, errors.New("dial tcp: postgres://user:hunter2@db/x refused")
	}
	_, err := run(t, newStdioServer(), stdioDeps(), frame(`{"jsonrpc": "2.0", "id": 1, "method": "ping"}`))
	if err == nil || strings.Contains(e.stderr.String(), "hunter2") || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("err=%v stderr=%q", err, e.stderr.String())
	}
	if !strings.Contains(e.stderr.String(), "MCP stdio refused:") {
		t.Fatalf("stderr %q", e.stderr.String())
	}
}

func TestStdioIgnoresPartialAndOversizedFrames(t *testing.T) {
	newStdioEnv(t)
	ping := `{"jsonrpc": "2.0", "id": 7, "method": "ping"}`
	// Garbage and unknown header lines are skipped; a frame cut short by EOF is dropped silently.
	got, err := run(t, newStdioServer(), stdioDeps(), "hello\r\nX-Foo: bar\r\n\r\n"+frame(ping)+"Content-Length: 50\r\n\r\n{\"jsonrpc\"")
	if err != nil {
		t.Fatal(err)
	}
	if b := parseFrames(t, got); len(b) != 1 || !strings.Contains(b[0], `"id": 7`) {
		t.Fatalf("got %q", got)
	}

	// A 5 MiB frame is refused with -32600 (streamed past, never buffered) and the next frame is served.
	big := 5 << 20
	in := fmt.Sprintf("Content-Length: %d\r\n\r\n", big) + strings.Repeat("a", big) + frame(ping)
	got, err = run(t, newStdioServer(), stdioDeps(), in)
	if err != nil {
		t.Fatal(err)
	}
	b := parseFrames(t, got)
	if len(b) != 2 || !strings.Contains(b[0], `"code": -32600`) || !strings.Contains(b[0], `"id": null`) || !strings.Contains(b[1], `"id": 7`) {
		t.Fatalf("got %q", got)
	}

	// A line-delimited line over the cap is refused too, then service continues.
	in = `{"x":"` + strings.Repeat("a", 5<<20) + "\"}\n" + ping + "\n"
	got, err = run(t, newStdioServer(), stdioDeps(), in)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"code": -32600`) || !strings.Contains(lines[1], `"id": 7`) {
		t.Fatalf("got %q", got)
	}
}

func TestStdioBadContentLengthDoesNotHang(t *testing.T) {
	newStdioEnv(t)
	ping := `{"jsonrpc": "2.0", "id": 9, "method": "ping"}`
	for _, bad := range []string{"abc", "-5", "+5", "1e3", "", "99999999999999999999999", "0x10"} {
		in := "Content-Length: " + bad + "\r\n\r\n" + frame(ping)
		got, err := run(t, newStdioServer(), stdioDeps(), in)
		if err != nil {
			t.Fatalf("%q: %v", bad, err)
		}
		b := parseFrames(t, got)
		if len(b) != 2 || !strings.Contains(b[0], `"code": -32600`) || !strings.Contains(b[1], `"id": 9`) {
			t.Fatalf("%q: got %q", bad, got)
		}
	}
}

func TestStdioHeaderTolerance(t *testing.T) {
	newStdioEnv(t)
	body := `{"jsonrpc": "2.0", "id": 3, "method": "ping"}`
	in := fmt.Sprintf("content-length: %d\r\nContent-Type: application/vscode-jsonrpc; charset=utf-8\r\n\r\n%s", len(body), body)
	got, err := run(t, newStdioServer(), stdioDeps(), in)
	if err != nil {
		t.Fatal(err)
	}
	if want := frame(`{"jsonrpc": "2.0", "id": 3, "result": {}}`); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// LF-only line endings too.
	in = fmt.Sprintf("Content-Length: %d\n\n%s", len(body), body)
	if got, _ = run(t, newStdioServer(), stdioDeps(), in); got != frame(`{"jsonrpc": "2.0", "id": 3, "result": {}}`) {
		t.Fatalf("got %q", got)
	}
}

func TestStdioNDJSONAutoDetect(t *testing.T) {
	newStdioEnv(t)
	in := `{"jsonrpc": "2.0", "id": 1, "method": "ping"}` + "\n" +
		frame(`{"jsonrpc": "2.0", "id": 2, "method": "ping"}`) +
		`{"jsonrpc": "2.0", "id": 3, "method": "ping"}` + "\r\n" +
		`{not json}` + "\n"
	got, err := run(t, newStdioServer(), stdioDeps(), in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"jsonrpc": "2.0", "id": 1, "result": {}}` + "\n" +
		frame(`{"jsonrpc": "2.0", "id": 2, "result": {}}`) +
		`{"jsonrpc": "2.0", "id": 3, "result": {}}` + "\n" +
		`{"jsonrpc": "2.0", "id": null, "error": {"code": -32700, "message": "Parse error"}}` + "\n"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestStdioNotificationNoReply(t *testing.T) {
	newStdioEnv(t)
	in := frame(`{"jsonrpc": "2.0", "method": "notifications/initialized"}`) +
		`{"jsonrpc": "2.0", "method": "notifications/cancelled"}` + "\n" +
		frame(`{"jsonrpc": "2.0", "id": 5, "method": "ping"}`)
	got, err := run(t, newStdioServer(), stdioDeps(), in)
	if err != nil {
		t.Fatal(err)
	}
	if b := parseFrames(t, got); len(b) != 1 || !strings.Contains(b[0], `"id": 5`) {
		t.Fatalf("got %q", got)
	}
}

func listReq(id int) string {
	return frame(fmt.Sprintf(`{"jsonrpc": "2.0", "id": %d, "method": "tools/list"}`, id))
}

// tickingClock advances 31s per call, so every tools/list|call is past the 30s revalidation window.
func tickingClock() {
	base := time.Unix(1_800_000_000, 0)
	var n int
	now = func() time.Time { n++; return base.Add(time.Duration(n) * 31 * time.Second) }
}

func TestStdioRevokedTokenEndsSession(t *testing.T) {
	e := newStdioEnv(t)
	tickingClock()
	e.resolv = func(_ string, n int) (Ctx, error) {
		if n >= 3 { // start + first revalidation succeed, the second fails
			return Ctx{}, &AuthError{Msg: "MCP token revoked"}
		}
		return stdioCtx(), nil
	}
	in := listReq(1) + listReq(2) + frame(`{"jsonrpc": "2.0", "id": 3, "method": "ping"}`) + listReq(4)
	got, err := run(t, newStdioServer(), stdioDeps(), in)
	if err == nil {
		t.Fatal("want non-nil error: session ended")
	}
	b := parseFrames(t, got)
	if len(b) != 2 || !strings.Contains(b[0], `"id": 1`) || !strings.Contains(b[0], `"tools"`) {
		t.Fatalf("got %q", got)
	}
	want := `{"jsonrpc": "2.0", "id": 2, "error": {"code": -32001, "message": "unauthorized: MCP token revoked"}}`
	if b[1] != want {
		t.Fatalf("got %q want %q", b[1], want)
	}
}

func TestStdioExpiredMidSessionStopsServing(t *testing.T) {
	e := newStdioEnv(t)
	tickingClock()
	e.resolv = func(_ string, n int) (Ctx, error) {
		if n >= 2 {
			return Ctx{}, &AuthError{Msg: "MCP token expired"}
		}
		return stdioCtx(), nil
	}
	call := frame(`{"jsonrpc": "2.0", "id": 8, "method": "tools/call", "params": {"name": "find_leads", "arguments": {}}}`)
	got, _ := run(t, newStdioServer(), stdioDeps(), call+call)
	b := parseFrames(t, got)
	if len(b) != 1 || !strings.Contains(b[0], `unauthorized: MCP token expired`) || !strings.Contains(b[0], `"id": 8`) {
		t.Fatalf("got %q", got)
	}
}

func TestStdioRevalidationIsRateLimitedAndRefreshesCtx(t *testing.T) {
	e := newStdioEnv(t)
	fixed := time.Unix(1_800_000_000, 0)
	now = func() time.Time { return fixed }
	if _, err := run(t, newStdioServer(), stdioDeps(), listReq(1)+listReq(2)+listReq(3)); err != nil {
		t.Fatal(err)
	}
	if e.nCalls() != 1 {
		t.Fatalf("resolver ran %d times inside the window", e.nCalls())
	}
	// Past the window the context is refreshed: a downgraded token loses its tools.
	tickingClock()
	e.calls = nil
	e.resolv = func(_ string, n int) (Ctx, error) {
		c := stdioCtx()
		if n > 2 { // start, first revalidation keep the grant; the second drops it
			c.Capabilities = map[string]bool{}
		}
		return c, nil
	}
	got, err := run(t, newStdioServer(), stdioDeps(), listReq(1)+listReq(2))
	if err != nil {
		t.Fatal(err)
	}
	b := parseFrames(t, got)
	if len(b) != 2 || !strings.Contains(b[0], "find_leads") || strings.Contains(b[1], "find_leads") {
		t.Fatalf("got %q", got)
	}
}

func TestStdioRevalidationDBErrorDoesNotKillSession(t *testing.T) {
	e := newStdioEnv(t)
	tickingClock()
	e.resolv = func(_ string, n int) (Ctx, error) {
		if n == 3 {
			return Ctx{}, errors.New("connection refused host=10.0.0.5 password=hunter2")
		}
		return stdioCtx(), nil
	}
	got, err := run(t, newStdioServer(), stdioDeps(), listReq(1)+listReq(2)+listReq(3))
	if err != nil {
		t.Fatal(err)
	}
	b := parseFrames(t, got)
	if len(b) != 3 || !strings.Contains(b[0], `"tools"`) || !strings.Contains(b[1], `"code": -32603`) || strings.Contains(got, "hunter2") || !strings.Contains(b[2], `"tools"`) {
		t.Fatalf("got %q", got)
	}
}

func TestStdioOutputOnlyProtocolBytes(t *testing.T) {
	e := newStdioEnv(t)
	_ = e
	s := newStdioServer()
	s.Execute = func(context.Context, Ctx, string, Args) string { panic("boom: " + stdioSecret) }
	in := frame(`{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}}`) +
		frame(`{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": "find_leads", "arguments": {}}}`) +
		"garbage line\r\n" + frame(`not json`) + frame(`[1,2]`) + frame(`{"jsonrpc": "2.0", "id": 4, "method": "nope"}`)
	got, err := run(t, s, stdioDeps(), in)
	if err != nil {
		t.Fatal(err)
	}
	b := parseFrames(t, got) // fails on any non-frame byte
	if len(b) != 5 {
		t.Fatalf("frames=%d: %q", len(b), got)
	}
	if strings.Contains(got, stdioSecret) {
		t.Fatal("token on stdout")
	}
	for _, body := range b {
		if !json.Valid([]byte(body)) {
			t.Fatalf("invalid JSON frame %q", body)
		}
	}
}

func TestStdioSequentialOrder(t *testing.T) {
	newStdioEnv(t)
	s := newStdioServer()
	var mu sync.Mutex
	active, maxActive := 0, 0
	s.Execute = func(_ context.Context, _ Ctx, name string, _ Args) string {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		if name == "slow" {
			time.Sleep(60 * time.Millisecond)
		}
		mu.Lock()
		active--
		mu.Unlock()
		return `{"name": "` + name + `"}`
	}
	var in strings.Builder
	for i := 1; i <= 12; i++ {
		name := "fast"
		if i%3 == 1 {
			name = "slow"
		}
		in.WriteString(frame(fmt.Sprintf(`{"jsonrpc": "2.0", "id": %d, "method": "tools/call", "params": {"name": %q, "arguments": {}}}`, i, name)))
	}
	got, err := run(t, s, stdioDeps(), in.String())
	if err != nil {
		t.Fatal(err)
	}
	b := parseFrames(t, got)
	if len(b) != 12 || maxActive != 1 {
		t.Fatalf("frames=%d maxActive=%d", len(b), maxActive)
	}
	for i, body := range b {
		if !strings.Contains(body, fmt.Sprintf(`"id": %d,`, i+1)) {
			t.Fatalf("out of order at %d: %q", i, body)
		}
	}
}

func TestStdioEOFReturnsNil(t *testing.T) {
	newStdioEnv(t)
	for _, in := range []string{"", "\r\n\r\n", frame(`{"jsonrpc": "2.0", "id": 1, "method": "ping"}`)} {
		if _, err := run(t, newStdioServer(), stdioDeps(), in); err != nil {
			t.Fatalf("%q: %v", in, err)
		}
	}
}

func TestStdioContextCancelReturnsNil(t *testing.T) {
	newStdioEnv(t)
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- ServeStdio(ctx, newStdioServer(), stdioDeps(), pr, &out, stdioSecret) }()
	_, _ = pw.Write([]byte(frame(`{"jsonrpc": "2.0", "id": 1, "method": "ping"}`)))
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not return on cancel")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestStdioWriteErrorEndsSession(t *testing.T) {
	newStdioEnv(t)
	err := ServeStdio(context.Background(), newStdioServer(), stdioDeps(),
		strings.NewReader(frame(`{"jsonrpc": "2.0", "id": 1, "method": "ping"}`)), failWriter{}, stdioSecret)
	if err == nil {
		t.Fatal("want write error")
	}
}
