//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/auth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// Task 29 tests: every user, workspace and workbook id carries the p6t29- prefix so the shared
// test database stays collision free.
const wsTag = "p6t29-"

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type wbSockEnv struct {
	mcpEnv
	srv  *httptest.Server
	logs *lockedBuf
	deps Deps
	// ids
	alice, bob, erin, inactive int32
	wsA, wsB                   string
	wb1, wb2, wbB              string
}

func newWBSockEnv(t *testing.T, mod func(*Deps)) *wbSockEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	logs := &lockedBuf{}
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Flags: config.Flags{PGLeadStore: true, GoRunRoutesEnabled: true},
		Log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	if mod != nil {
		mod(&d)
	}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), httpx.Gzip(1), audit.Middleware(pool, d.Log))
	RegisterWorkbookWS(r, d)
	e := &wbSockEnv{mcpEnv: mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}, logs: logs, deps: d}
	e.srv = httptest.NewServer(e.h)
	t.Cleanup(e.srv.Close)

	e.seed(t, wsTag+"alice", pythonHash, true, "admin", false)
	e.seed(t, wsTag+"bob", pythonHash, true, "user", false)
	e.seed(t, wsTag+"erin", pythonHash, true, "user", false)
	e.seed(t, wsTag+"inactive", pythonHash, false, "user", false)
	e.alice, e.bob = e.mustUserByName(t, wsTag+"alice").ID, e.mustUserByName(t, wsTag+"bob").ID
	e.erin, e.inactive = e.mustUserByName(t, wsTag+"erin").ID, e.mustUserByName(t, wsTag+"inactive").ID
	e.wsA, e.wsB = wsTag+"ws-alpha", wsTag+"ws-beta"
	e.workspace(t, e.wsA, e.alice)
	e.member(t, e.wsA, e.alice, "owner")
	e.member(t, e.wsA, e.bob, "viewer")
	e.member(t, e.wsA, e.erin, "editor")
	e.workspace(t, e.wsB, e.bob)
	e.member(t, e.wsB, e.bob, "owner")
	e.wb1, e.wb2, e.wbB = wsTag+"wb-enrich-1", wsTag+"wb-alpha-2", wsTag+"wb-beta-1"
	e.workbook(t, e.wsA, e.wb1)
	e.workbook(t, e.wsA, e.wb2)
	e.workbook(t, e.wsB, e.wbB)
	return e
}

func (e *wbSockEnv) workbook(t *testing.T, ws, id string) {
	t.Helper()
	c := context.Background()
	err := tenancy.InWorkspace(c, e.pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(c, `INSERT INTO workbooks (id, name, workspace_id) VALUES ($1,$1,$2)`, id, ws)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tenancy.InWorkspace(c, e.pool, ws, func(tx pgx.Tx) error {
			_, err := tx.Exec(c, `DELETE FROM workbooks WHERE id=$1`, id)
			return err
		})
	})
}

func (e *wbSockEnv) token(t *testing.T, user string) string {
	t.Helper()
	tok, err := testSigner.Access(wsTag+user, []string{"pwd"})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// dial connects to /api/workbooks/{wb}/ws?query. On a refused handshake err is non-nil and the
// response carries the HTTP status.
func (e *wbSockEnv) dial(wb, query string) (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/api/workbooks/" + wb + "/ws"
	if query != "" {
		u += "?" + query
	}
	return websocket.Dial(ctx, u, nil)
}

func (e *wbSockEnv) mustDial(t *testing.T, wb, user, ws string) *websocket.Conn {
	t.Helper()
	c, resp, err := e.dial(wb, "token="+e.token(t, user)+"&workspace_id="+ws)
	if err != nil {
		st := 0
		if resp != nil {
			st = resp.StatusCode
		}
		t.Fatalf("dial: %v (status %d)", err, st)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func readText(t *testing.T, c *websocket.Conn, within time.Duration) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// pump reads frames on its own goroutine: a coder/websocket Read whose context expires closes the
// connection, so "expect nothing for a while" checks must not use a short-lived read context.
type pump struct{ ch chan string }

func newPump(c *websocket.Conn) *pump {
	p := &pump{ch: make(chan string, 64)}
	go func() {
		defer close(p.ch)
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				return
			}
			p.ch <- string(b)
		}
	}()
	return p
}

func (p *pump) next(within time.Duration) (string, bool) {
	select {
	case m, ok := <-p.ch:
		return m, ok
	case <-time.After(within):
		return "", false
	}
}

func sendText(t *testing.T, c *websocket.Conn, s string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(s)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func handshakeStatus(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func TestWSRejectsMissingTokenOrWorkspace4403(t *testing.T) {
	e := newWBSockEnv(t, nil)
	tok := e.token(t, "alice")
	for name, q := range map[string]string{
		"no token":     "workspace_id=" + e.wsA,
		"no workspace": "token=" + tok,
		"neither":      "",
		"empty token":  "token=&workspace_id=" + e.wsA,
		"garbage":      "token=garbage&workspace_id=" + e.wsA,
	} {
		_, resp, err := e.dial(e.wb1, q)
		if err == nil || handshakeStatus(resp) != 403 {
			t.Errorf("%s: err=%v status=%d, want refused handshake 403", name, err, handshakeStatus(resp))
		}
	}
	// A refresh token and an inactive user are refused too.
	ref, _ := testSigner.Refresh(wsTag+"alice", nil)
	inact, _ := testSigner.Access(wsTag+"inactive", nil)
	for name, tk := range map[string]string{"refresh": ref, "inactive": inact} {
		_, resp, err := e.dial(e.wb1, "token="+tk+"&workspace_id="+e.wsA)
		if err == nil || handshakeStatus(resp) != 403 {
			t.Errorf("%s: err=%v status=%d", name, err, handshakeStatus(resp))
		}
	}
	// Starlette maps the pre-accept close(4403) to an HTTP 403 handshake; both are asserted.
	if wsCloseForbidden != 4403 || handshakeStatusForClose(wsCloseForbidden) != http.StatusForbidden {
		t.Errorf("close code %d maps to %d", wsCloseForbidden, handshakeStatusForClose(wsCloseForbidden))
	}
}

func TestWSRejectsNonMemberAndForeignWorkbook4403(t *testing.T) {
	e := newWBSockEnv(t, nil)
	cases := map[string]struct{ wb, user, ws string }{
		"forged workspace (alice not in beta)": {e.wb1, "alice", e.wsB},
		"unknown workspace":                    {e.wb1, "alice", wsTag + "ws-nope"},
		"foreign workbook (alpha wb via beta)": {e.wb1, "bob", e.wsB},
		"workbook of another tenant":           {e.wbB, "alice", e.wsA},
		"unknown workbook":                     {wsTag + "missing", "alice", e.wsA},
	}
	for name, c := range cases {
		_, resp, err := e.dial(c.wb, "token="+e.token(t, c.user)+"&workspace_id="+c.ws)
		if err == nil || handshakeStatus(resp) != 403 {
			t.Errorf("%s: err=%v status=%d", name, err, handshakeStatus(resp))
		}
	}
	// SSO enforcement: a non-owner without the sso amr is refused, the owner is let in.
	setSetting(t, e.pool, e.wsA, "OIDC_CONFIG", `{"enforce_sso": true}`)
	_, resp, err := e.dial(e.wb1, "token="+e.token(t, "erin")+"&workspace_id="+e.wsA)
	if err == nil || handshakeStatus(resp) != 403 {
		t.Errorf("sso non-owner: err=%v status=%d", err, handshakeStatus(resp))
	}
	e.mustDial(t, e.wb1, "alice", e.wsA)
}

func TestWSPingPong(t *testing.T) {
	e := newWBSockEnv(t, nil)
	c := e.mustDial(t, e.wb1, "bob", e.wsA) // a non-editor member may connect (Python: membership only)
	sendText(t, c, `{"type":"ping"}`)
	if got, ok := readText(t, c, 2*time.Second); !ok || got != `{"type":"pong"}` {
		t.Fatalf("pong = %q %v", got, ok)
	}
	sendText(t, c, `{"type":"other"}`)
	sendText(t, c, `{"type":"ping","x":1}`)
	if got, ok := readText(t, c, 2*time.Second); !ok || got != `{"type":"pong"}` {
		t.Fatalf("second pong = %q %v (an unknown type must not answer)", got, ok)
	}
}

func TestWSInvalidJSONIgnored(t *testing.T) {
	e := newWBSockEnv(t, nil)
	c := e.mustDial(t, e.wb1, "alice", e.wsA)
	// Python's json.loads raises and the socket drops (1006); Go ignores the frame (documented hardening).
	for _, bad := range []string{"not json", `[1,2]`, `"ping"`, `null`, `{"type":`} {
		sendText(t, c, bad)
	}
	sendText(t, c, `{"type":"ping"}`)
	if got, ok := readText(t, c, 2*time.Second); !ok || got != `{"type":"pong"}` {
		t.Fatalf("after invalid frames: %q %v", got, ok)
	}
}

func TestWSRedisDownEchoMode(t *testing.T) {
	for name, rc := range map[string]*redis.Client{
		"nil client":  nil,
		"unreachable": redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond}),
	} {
		t.Run(name, func(t *testing.T) {
			e := newWBSockEnv(t, func(d *Deps) { d.Redis = rc })
			c := e.mustDial(t, e.wb1, "alice", e.wsA)
			sendText(t, c, `{"type":"ping"}`)
			if got, ok := readText(t, c, 5*time.Second); !ok || got != `{"type":"pong"}` {
				t.Fatalf("echo pong = %q %v", got, ok)
			}
		})
	}
}

func TestWSRelaysRedisPayloadsVerbatim(t *testing.T) {
	rc := testRedis(t)
	e := newWBSockEnv(t, func(d *Deps) { d.Redis = rc })
	c := e.mustDial(t, e.wb1, "alice", e.wsA)
	ctx := context.Background()
	other := `{"type":"cell_update","workbook_id":"other"}`
	msgs := []string{
		`{"type":"cell_update","workbook_id":"` + e.wb1 + `","row_id":101,"column_id":"calc_len","value":"6"}`,
		"{\"type\":\"progress\",\"note\":\"unicode é東 and  spaces\"}",
		"plain text not json",
		"",
	}
	// A message for another workbook must never arrive (per-workbook channel only).
	if err := rc.Publish(ctx, "workbook:"+e.wb2, other).Err(); err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if err := rc.Publish(ctx, "workbook:"+e.wb1, m).Err(); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range msgs {
		typ, b, err := readMsg(c, 3*time.Second)
		if err != nil || typ != websocket.MessageText || string(b) != want {
			t.Fatalf("msg %d = %v %q %v, want text %q", i, typ, b, err, want)
		}
	}
	pm := newPump(c)
	if got, ok := pm.next(300 * time.Millisecond); ok {
		t.Fatalf("unexpected extra frame %q", got)
	}
}

func readMsg(c *websocket.Conn, within time.Duration) (websocket.MessageType, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	return c.Read(ctx)
}

func TestWSOversizedFrameClosed(t *testing.T) {
	e := newWBSockEnv(t, func(d *Deps) { d.WSMaxMessageBytes = 64 })
	c := e.mustDial(t, e.wb1, "alice", e.wsA)
	sendText(t, c, `{"type":"ping"}`) // under the limit
	if _, ok := readText(t, c, 2*time.Second); !ok {
		t.Fatal("small frame not answered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"ping","pad":"`+strings.Repeat("x", 200)+`"}`))
	_, _, err := c.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusMessageTooBig {
		t.Fatalf("close status = %d (%v), want 1009", got, err)
	}
}

func goroutines() int { runtime.GC(); return runtime.NumGoroutine() }

func TestWSDisconnectReleasesSubscription(t *testing.T) {
	rc := testRedis(t)
	e := newWBSockEnv(t, func(d *Deps) { d.Redis = rc })
	warm := e.mustDial(t, e.wb1, "alice", e.wsA)
	_ = warm.Close(websocket.StatusNormalClosure, "")
	time.Sleep(300 * time.Millisecond)
	base := goroutines()
	for i := 0; i < 5; i++ { // reconnect storm
		c := e.mustDial(t, e.wb1, "alice", e.wsA)
		sendText(t, c, `{"type":"ping"}`)
		readText(t, c, 2*time.Second)
		_ = c.CloseNow()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		subs, _ := rc.PubSubNumSub(context.Background(), "workbook:"+e.wb1).Result()
		if subs["workbook:"+e.wb1] == 0 && goroutines() <= base+1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("subscriptions=%v goroutines=%d (baseline %d)", subs, goroutines(), base)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestWSStreamCap503(t *testing.T) {
	e := newWBSockEnv(t, func(d *Deps) { d.SSEMaxStreams = 1 })
	c := e.mustDial(t, e.wb1, "alice", e.wsA)
	_, resp, err := e.dial(e.wb1, "token="+e.token(t, "erin")+"&workspace_id="+e.wsA)
	if err == nil || handshakeStatus(resp) != 503 {
		t.Fatalf("second stream: err=%v status=%d, want 503", err, handshakeStatus(resp))
	}
	_ = c.CloseNow() // the slot is released on disconnect
	deadline := time.Now().Add(3 * time.Second)
	for {
		c2, _, err := e.dial(e.wb1, "token="+e.token(t, "erin")+"&workspace_id="+e.wsA)
		if err == nil {
			_ = c2.CloseNow()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot never released: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestWSPerUserAndWorkspaceCaps429(t *testing.T) {
	e := newWBSockEnv(t, func(d *Deps) { d.SSEMaxPerUser, d.SSEMaxPerWorkspace = 1, 2 })
	e.mustDial(t, e.wb1, "alice", e.wsA)
	_, resp, err := e.dial(e.wb2, "token="+e.token(t, "alice")+"&workspace_id="+e.wsA)
	if err == nil || handshakeStatus(resp) != 429 {
		t.Fatalf("per-user: err=%v status=%d", err, handshakeStatus(resp))
	}
	e.mustDial(t, e.wb1, "erin", e.wsA)
	_, resp, err = e.dial(e.wb1, "token="+e.token(t, "bob")+"&workspace_id="+e.wsA)
	if err == nil || handshakeStatus(resp) != 429 {
		t.Fatalf("per-workspace: err=%v status=%d", err, handshakeStatus(resp))
	}
}

func TestWSMaxLifetimeClosesSocket(t *testing.T) {
	e := newWBSockEnv(t, func(d *Deps) { d.SSEMaxLifetime = 300 * time.Millisecond })
	c := e.mustDial(t, e.wb1, "alice", e.wsA)
	_, _, err := readMsg(c, 3*time.Second)
	if got := websocket.CloseStatus(err); got != websocket.StatusGoingAway {
		t.Fatalf("lifetime close = %d (%v), want 1001", got, err)
	}
}

func TestWSShutdownClosesSockets(t *testing.T) {
	sctx, stop := context.WithCancel(context.Background())
	defer stop()
	e := newWBSockEnv(t, func(d *Deps) { d.ShutdownCtx = sctx })
	c := e.mustDial(t, e.wb1, "alice", e.wsA)
	stop()
	_, _, err := readMsg(c, 3*time.Second)
	if got := websocket.CloseStatus(err); got != websocket.StatusGoingAway {
		t.Fatalf("shutdown close = %d (%v), want 1001", got, err)
	}
}

func TestWSUnresponsiveClientDropped(t *testing.T) {
	oldI, oldT := wsPingInterval, wsPongTimeout
	wsPingInterval, wsPongTimeout = 50*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { wsPingInterval, wsPongTimeout = oldI, oldT })
	e := newWBSockEnv(t, func(d *Deps) { d.SSEMaxStreams = 1 })
	// This client never reads, so it never answers the server's pings.
	c := e.mustDial(t, e.wb1, "alice", e.wsA)
	_ = c
	deadline := time.Now().Add(5 * time.Second)
	for {
		c2, _, err := e.dial(e.wb1, "token="+e.token(t, "erin")+"&workspace_id="+e.wsA)
		if err == nil {
			_ = c2.CloseNow()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("unresponsive client still holds its stream slot")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestWSNeverLogsToken(t *testing.T) {
	e := newWBSockEnv(t, nil)
	tok := e.token(t, "alice")
	c := e.mustDial(t, e.wb1, "alice", e.wsA)
	sendText(t, c, `{"type":"ping"}`)
	readText(t, c, 2*time.Second)
	_ = c.CloseNow()
	_, _, _ = e.dial(e.wb1, "token="+tok+"x&workspace_id="+e.wsA)
	_, _, _ = e.dial(e.wb1, "token="+tok+"&workspace_id="+e.wsB)
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(e.logs.String(), tok[:20]) || strings.Contains(e.logs.String(), "token=") {
		t.Fatalf("logs leak the query token:\n%s", e.logs.String())
	}
}

func TestWSOriginCheck(t *testing.T) {
	e := newWBSockEnv(t, func(d *Deps) { d.Config.CORSOrigins = "https://app.example.com" })
	tok := e.token(t, "alice")
	dialOrigin := func(origin string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		u := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/api/workbooks/" + e.wb1 + "/ws?token=" + tok + "&workspace_id=" + e.wsA
		c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {origin}}})
		if err == nil {
			_ = c.CloseNow()
		}
		return err
	}
	if err := dialOrigin("https://app.example.com"); err != nil {
		t.Errorf("configured origin refused: %v", err)
	}
	if err := dialOrigin("https://evil.example.net"); err == nil {
		t.Error("foreign origin accepted")
	}
}

// wsFixture is testdata/contract/ws/*.json (recorded from FastAPI).
type wsFixture struct {
	Request struct {
		As    *string           `json:"as"`
		Path  string            `json:"path"`
		Query map[string]string `json:"query"`
		Steps []map[string]any  `json:"steps"`
		Token string            `json:"token"`
	} `json:"request"`
	Response struct {
		Closed *int `json:"closed_by_server"`
		Status int  `json:"handshake_status"`
		Steps  []struct {
			Received []string `json:"received"`
		} `json:"steps"`
	} `json:"response"`
}

func TestWSMatchesFixtures(t *testing.T) {
	files, _ := filepath.Glob("../../testdata/contract/ws/*.json")
	if len(files) == 0 {
		t.Fatal("no ws fixtures")
	}
	var rc *redis.Client
	if os.Getenv("TEST_REDIS_URL") != "" {
		rc = testRedis(t)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		t.Run(name, func(t *testing.T) {
			redisCase := strings.HasPrefix(name, "ws_redis_")
			if redisCase && rc == nil {
				t.Skip("TEST_REDIS_URL not set")
			}
			e := newWBSockEnv(t, func(d *Deps) {
				if redisCase {
					d.Redis = rc
				}
			})
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			// Map the recorded ids onto this test's unique ones, in the requests and the expected frames.
			s := string(raw)
			for from, to := range map[string]string{"ws-alpha": e.wsA, "ws-beta": e.wsB, "ws-nope": wsTag + "ws-nope",
				"wb-enrich-1": e.wb1, "wb-alpha-2": e.wb2} {
				s = strings.ReplaceAll(s, from, to)
			}
			var fx wsFixture
			if err := json.Unmarshal([]byte(s), &fx); err != nil {
				t.Fatal(err)
			}
			q := []string{}
			for k, v := range fx.Request.Query {
				if k == "token" {
					continue
				}
				q = append(q, k+"="+v)
			}
			switch fx.Request.Token {
			case "login":
				q = append(q, "token="+e.token(t, *fx.Request.As))
			case "expired":
				old := auth.Signer{Secret: testSigner.Secret, AccessTTL: -time.Hour}
				tk, _ := old.Access(wsTag+"alice", nil)
				q = append(q, "token="+tk)
			case "refresh":
				tk, _ := testSigner.Refresh(wsTag+"alice", nil)
				q = append(q, "token="+tk)
			case "inactive":
				tk, _ := testSigner.Access(wsTag+"inactive", nil)
				q = append(q, "token="+tk)
			case "garbage":
				q = append(q, "token=garbage")
			}
			path := strings.TrimSuffix(strings.TrimPrefix(fx.Request.Path, "/api/workbooks/"), "/ws")
			c, resp, err := e.dial(path, strings.Join(q, "&"))
			if fx.Response.Status != 101 {
				// FastAPI answers a pre-accept close(4403) with an HTTP 403 handshake.
				if err == nil || handshakeStatus(resp) != fx.Response.Status {
					t.Fatalf("handshake = %d err=%v, want %d", handshakeStatus(resp), err, fx.Response.Status)
				}
				return
			}
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.CloseNow()
			pm := newPump(c)
			// Documented deviation: FastAPI drops the socket (1006) on invalid JSON in echo mode; Go ignores
			// the frame and keeps serving, so the following ping is answered.
			want := fx.Response.Steps
			if name == "ws_bad_json_then_ping" {
				want = []struct {
					Received []string `json:"received"`
				}{{Received: []string{}}, {Received: []string{`{"type":"pong"}`}}}
			}
			if name == "ws_redis_bad_json_then_ping" {
				// Python's invalid-JSON exception also drops the Redis relay (it falls into echo mode, so the
				// later publish is lost); Go keeps relaying.
				want[2].Received = []string{`{"type":"cell_update","workbook_id":"` + e.wb1 + `","row_id":101,"column_id":"calc_len","value":"6"}`}
			}
			for i, step := range fx.Request.Steps {
				if m, ok := step["send"].(string); ok {
					sendText(t, c, m)
				}
				if p, ok := step["publish"].(map[string]any); ok {
					if err := rc.Publish(context.Background(), p["channel"].(string), p["message"].(string)).Err(); err != nil {
						t.Fatal(err)
					}
				}
				got := []string{}
				for {
					m, ok := pm.next(250 * time.Millisecond)
					if !ok {
						break
					}
					got = append(got, m)
				}
				if !equalStrings(got, want[i].Received) {
					t.Fatalf("step %d received %q, want %q", i, got, want[i].Received)
				}
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var _ = errors.New
var _ = io.Discard
