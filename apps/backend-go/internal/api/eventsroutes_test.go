//go:build integration

package api

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type eventsEnv struct {
	mcpEnv
	deps Deps
}

func newEventsEnv(t *testing.T, mod func(*Deps)) eventsEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Flags: config.Flags{PGLeadStore: true}, Log: quietLog()}
	if mod != nil {
		mod(&d)
	}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil), httpx.Gzip(1))
	RegisterEvents(r, d)
	e := eventsEnv{mcpEnv: mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}, deps: d}
	return e
}

// users alice (owner of ws) and bob (not a member) with unique names/ids per test.
func (e eventsEnv) seedWS(t *testing.T, tag string) (ws string) {
	t.Helper()
	e.seed(t, tag+"-alice", pythonHash, true, "admin", true)
	e.seed(t, tag+"-bob", pythonHash, true, "user", false)
	a := e.mustUserByName(t, tag+"-alice")
	ws = tag + "-ws"
	e.workspace(t, ws, a.ID)
	e.member(t, ws, a.ID, "owner")
	return ws
}

func setSetting(t *testing.T, pool *pgxpool.Pool, ws, key, val string) {
	t.Helper()
	c := context.Background()
	err := tenancy.InWorkspace(c, pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(c, `INSERT INTO workspace_settings (workspace_id,key,value) VALUES ($1,$2,$3)`, ws, key, val)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tenancy.InWorkspace(c, pool, ws, func(tx pgx.Tx) error {
			_, err := tx.Exec(c, `DELETE FROM workspace_settings WHERE workspace_id=$1`, ws)
			return err
		})
	})
}

func (e eventsEnv) tokenQuery(t *testing.T, user string, amr []string) string {
	t.Helper()
	tok, err := testSigner.Access(user, amr)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (e eventsEnv) get(path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestEventsMissingToken401(t *testing.T) {
	e := newEventsEnv(t, nil)
	for _, p := range []string{"/api/events", "/api/events?token="} {
		rec := e.get(p)
		if rec.Code != 401 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Missing token"}` {
			t.Errorf("%s = %d %s", p, rec.Code, rec.Body)
		}
	}
	// header-only auth is not accepted
	req := httptest.NewRequest("GET", "/api/events", nil)
	for k, v := range bearer("nobody") {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "Missing token") {
		t.Errorf("header only = %d %s", rec.Code, rec.Body)
	}
	if rec := e.get("/api/events?token=not.a.token"); rec.Code != 401 || !strings.Contains(rec.Body.String(), "Invalid token") {
		t.Errorf("bad token = %d %s", rec.Code, rec.Body)
	}
}

func TestEventsRefreshTokenRejected401(t *testing.T) {
	e := newEventsEnv(t, nil)
	ws := e.seedWS(t, "t16r")
	_ = ws
	tok, _ := testSigner.Refresh("t16r-alice", []string{"pwd"})
	rec := e.get("/api/events?token=" + tok)
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "Invalid token") {
		t.Fatalf("refresh token = %d %s", rec.Code, rec.Body)
	}
}

func TestEventsNonMemberWorkspace403(t *testing.T) {
	e := newEventsEnv(t, nil)
	ws := e.seedWS(t, "t16m")
	tok := e.tokenQuery(t, "t16m-bob", []string{"pwd"})
	for _, w := range []string{ws, "t16m-nope"} {
		rec := e.get("/api/events?token=" + tok + "&workspace_id=" + w)
		if rec.Code != 403 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Workspace access denied"}` {
			t.Errorf("ws %s = %d %s", w, rec.Code, rec.Body)
		}
	}
}

func TestEventsNoActiveWorkspace403(t *testing.T) {
	e := newEventsEnv(t, nil)
	e.seedWS(t, "t16n")
	tok := e.tokenQuery(t, "t16n-bob", []string{"pwd"}) // bob has no workspace at all
	rec := e.get("/api/events?token=" + tok)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "Workspace access denied") {
		t.Fatalf("= %d %s", rec.Code, rec.Body)
	}
}

func TestEventsSSORequired403(t *testing.T) {
	e := newEventsEnv(t, nil)
	ws := e.seedWS(t, "t16s")
	bob := e.mustUserByName(t, "t16s-bob")
	e.member(t, ws, bob.ID, "member")
	setSetting(t, e.pool, ws, "OIDC_CONFIG", `{"enforce_sso":true}`)
	want := `{"detail":"This workspace requires SSO. Sign in with your organization identity."}`
	rec := e.get("/api/events?token=" + e.tokenQuery(t, "t16s-bob", []string{"pwd"}))
	if rec.Code != 403 || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("pwd member = %d %s", rec.Code, rec.Body)
	}
	// owner is exempt and an sso token passes: both reach the stream (cancel at once)
	for _, c := range []struct {
		user string
		amr  []string
	}{{"t16s-alice", []string{"pwd"}}, {"t16s-bob", []string{"sso"}}} {
		resp, _, _ := openStream(t, e, "/api/events?token="+e.tokenQuery(t, c.user, c.amr), nil)
		if resp.StatusCode != 200 {
			t.Errorf("%s %v = %d", c.user, c.amr, resp.StatusCode)
		}
	}
}

// openStream serves the request on a real server and returns the response.
func openStream(t *testing.T, e eventsEnv, path string, hdr map[string]string) (*http.Response, context.CancelFunc, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(e.h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp, cancel, srv
}

func TestEventsHeadersAndFlush(t *testing.T) { headersCheck(t, "t16h") }

func headersCheck(t *testing.T, tag string) {
	setHeartbeat(t, 50*time.Millisecond)
	e := newEventsEnv(t, nil)
	ws := e.seedWS(t, tag)
	tok := e.tokenQuery(t, tag+"-alice", []string{"pwd"})
	resp, _, _ := openStream(t, e, "/api/events?token="+tok+"&workspace_id="+ws, map[string]string{"Accept-Encoding": "gzip"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for k, want := range map[string]string{
		"Content-Type":      "text/event-stream; charset=utf-8",
		"Cache-Control":     "no-cache",
		"X-Accel-Buffering": "no",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("gzip applied to event stream: %q", ce)
	}
	// the first heartbeat arrives promptly (flushed per frame, not buffered)
	line := make(chan string, 1)
	go func() { s, _ := bufio.NewReader(resp.Body).ReadString('\n'); line <- s }()
	select {
	case s := <-line:
		if s != "data: {\"type\": \"heartbeat\"}\n" {
			t.Errorf("first line %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no flushed heartbeat")
	}
}

func TestEventsHeartbeat(t *testing.T) { headersCheck(t, "t16hb") }

func TestEventsStreamCap503(t *testing.T) { capCheck(t, "t16c") }

func capCheck(t *testing.T, tag string) {
	setHeartbeat(t, time.Hour)
	e := newEventsEnv(t, func(d *Deps) { d.SSEMaxStreams = 1 })
	ws := e.seedWS(t, tag)
	tok := e.tokenQuery(t, tag+"-alice", []string{"pwd"})
	path := "/api/events?token=" + tok + "&workspace_id=" + ws
	r1, cancel1, srv := openStream(t, e, path, nil)
	if r1.StatusCode != 200 {
		t.Fatalf("first = %d", r1.StatusCode)
	}
	get := func() (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		return resp.StatusCode, b.String()
	}
	if c, b := get(); c != 503 || strings.TrimSpace(b) != `{"detail":"Too many event streams"}` {
		t.Fatalf("second = %d %s", c, b)
	}
	cancel1()
	r1.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, _ := func() (int, string) {
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			return resp.StatusCode, ""
		}()
		if c == 200 {
			break // slot released after disconnect
		}
		if time.Now().After(deadline) {
			t.Fatal("slot not released after client disconnect")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestEventsClientDisconnectReleasesSlot(t *testing.T) { capCheck(t, "t16d") }

func TestEventsGzipSkippedBehindMiddleware(t *testing.T) { headersCheck(t, "t16gz") }

func TestEventsTokenNotLogged(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := newEventsEnv(t, func(d *Deps) { d.Log = log })
	ws := e.seedWS(t, "t16l")
	tok := e.tokenQuery(t, "t16l-alice", []string{"pwd"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, p := range []string{"/api/events?token=" + tok + "&workspace_id=" + ws, "/api/events?token=" + tok + "&workspace_id=nope"} {
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil).WithContext(ctx))
	}
	if strings.Contains(buf.String(), tok) {
		t.Fatal("token leaked into logs")
	}
}

func setHeartbeat(t *testing.T, d time.Duration) {
	old := sseHeartbeat
	sseHeartbeat = d
	t.Cleanup(func() { sseHeartbeat = old })
}

// A shutdown signal must end open streams so http.Server.Shutdown does not wait out its grace.
func TestEventsStreamEndsOnShutdown(t *testing.T) {
	setHeartbeat(t, time.Hour)
	sctx, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	e := newEventsEnv(t, func(d *Deps) { d.ShutdownCtx = sctx })
	ws := e.seedWS(t, "t16sd")
	tok := e.tokenQuery(t, "t16sd-alice", []string{"pwd"})
	resp, _, _ := openStream(t, e, "/api/events?token="+tok+"&workspace_id="+ws, nil)
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, resp.Body); close(done) }()
	select {
	case <-done:
		t.Fatal("stream ended before shutdown")
	case <-time.After(150 * time.Millisecond):
	}
	shutdown()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream still open after shutdown signal")
	}
}

func TestEventsPerUserCap429(t *testing.T) {
	setHeartbeat(t, time.Hour)
	e := newEventsEnv(t, func(d *Deps) { d.SSEMaxPerUser = 1 })
	ws := e.seedWS(t, "t16u")
	tok := e.tokenQuery(t, "t16u-alice", []string{"pwd"})
	path := "/api/events?token=" + tok + "&workspace_id=" + ws
	r1, cancel1, srv := openStream(t, e, path, nil)
	if r1.StatusCode != 200 {
		t.Fatalf("first = %d", r1.StatusCode)
	}
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 429 || strings.TrimSpace(b.String()) != `{"detail":"Too many event streams for this user"}` {
		t.Fatalf("second = %d %s", resp.StatusCode, b.String())
	}
	cancel1()
	r1.Body.Close()
	waitStreamOpens(t, srv.URL+path)
}

func TestEventsPerWorkspaceCap429(t *testing.T) {
	setHeartbeat(t, time.Hour)
	e := newEventsEnv(t, func(d *Deps) { d.SSEMaxPerWorkspace = 1 })
	ws := e.seedWS(t, "t16w")
	tok := e.tokenQuery(t, "t16w-alice", []string{"pwd"})
	path := "/api/events?token=" + tok + "&workspace_id=" + ws
	r1, cancel1, srv := openStream(t, e, path, nil)
	if r1.StatusCode != 200 {
		t.Fatalf("first = %d", r1.StatusCode)
	}
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 429 || strings.TrimSpace(b.String()) != `{"detail":"Too many event streams for this workspace"}` {
		t.Fatalf("second = %d %s", resp.StatusCode, b.String())
	}
	cancel1()
	r1.Body.Close()
	waitStreamOpens(t, srv.URL+path)
}

// waitStreamOpens polls until a new stream is accepted (a slot was released).
func waitStreamOpens(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == 200 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("slot not released")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// A stream is closed after its maximum lifetime so the client reconnects and re-authenticates.
func TestEventsMaxLifetimeClosesStream(t *testing.T) {
	setHeartbeat(t, time.Hour)
	e := newEventsEnv(t, func(d *Deps) { d.SSEMaxLifetime = 200 * time.Millisecond })
	ws := e.seedWS(t, "t16lt")
	tok := e.tokenQuery(t, "t16lt-alice", []string{"pwd"})
	resp, _, _ := openStream(t, e, "/api/events?token="+tok+"&workspace_id="+ws, nil)
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, resp.Body); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream outlived its max lifetime")
	}
}
