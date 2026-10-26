//go:build integration

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/personintel"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const pwsTag = "p12t20-"

type pwsEnv struct {
	mcpEnv
	srv  *httptest.Server
	logs *lockedBuf
	w    *personWS
}

type pwsEnrich = func(ctx context.Context, d personintel.Deps, url string, deep bool, notify func(string, map[string]any)) (personintel.Profile, error)

func newPWSEnv(t *testing.T, enrich pwsEnrich, mod func(*Deps)) *pwsEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	logs := &lockedBuf{}
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Flags: config.Flags{PGLeadStore: true, Copilot: config.CopilotSettings{GoCopilotEnabled: true}},
		Egress: &http.Client{Transport: pwsEmptyWeb{}},
		Log:    slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	if mod != nil {
		mod(&d)
	}
	w := newPersonWS(d)
	if enrich != nil {
		w.enrich = enrich
	}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID())
	w.register(r)
	e := &pwsEnv{mcpEnv: mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}, logs: logs, w: w}
	e.srv = httptest.NewServer(e.h)
	t.Cleanup(e.srv.Close)
	e.seed(t, pwsTag+"admin", pythonHash, true, "admin", true)
	e.seed(t, pwsTag+"admin2", pythonHash, true, "admin", true)
	e.seed(t, pwsTag+"user", pythonHash, true, "user", false)
	ownerExec(t, `TRUNCATE person_intel RESTART IDENTITY`)
	t.Cleanup(func() { wcrudOwnerExecQuiet(`TRUNCATE person_intel RESTART IDENTITY`) })
	return e
}

type pwsEmptyWeb struct{}

func (pwsEmptyWeb) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("<html></html>")), Request: r}, nil
}

func (e *pwsEnv) token(t *testing.T, user string) string {
	t.Helper()
	tok, err := testSigner.Access(pwsTag+user, []string{"pwd"})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (e *pwsEnv) dial(query string) (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws/person/enrich"
	if query != "" {
		u += "?" + query
	}
	return websocket.Dial(ctx, u, nil)
}

func (e *pwsEnv) mustDial(t *testing.T, user string) *websocket.Conn {
	t.Helper()
	c, resp, err := e.dial("token=" + e.token(t, user))
	if err != nil {
		t.Fatalf("dial: %v (status %d)", err, handshakeStatus(resp))
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

// readAll reads text frames until the server closes; it returns them and the close status.
func readAll(t *testing.T, c *websocket.Conn, within time.Duration) ([]string, websocket.StatusCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	var out []string
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			return out, websocket.CloseStatus(err)
		}
		out = append(out, string(b))
	}
}

const pwsURL = `{"linkedin_url":"https://www.linkedin.com/in/jane-doe","deep":false}`

func fakeProfile(u string) personintel.Profile {
	p := personintel.NewProfile(u)
	p.Username, p.Name, p.Status = "jane-doe", "Jane <Doe> & Co", "completed"
	return *p
}

func TestPersonWSRejectsBadTokenOrNonAdmin4403(t *testing.T) {
	e := newPWSEnv(t, nil, nil)
	ref, _ := testSigner.Refresh(pwsTag+"admin", nil)
	for name, q := range map[string]string{
		"no token": "", "empty": "token=", "garbage": "token=garbage",
		"non-admin": "token=" + e.token(t, "user"), "refresh": "token=" + ref,
	} {
		_, resp, err := e.dial(q)
		if err == nil || handshakeStatus(resp) != http.StatusForbidden {
			t.Errorf("%s: err=%v status=%d, want refused handshake 403", name, err, handshakeStatus(resp))
		}
	}
	if strings.Contains(e.logs.String(), "garbage") || strings.Contains(e.logs.String(), e.token(t, "user")) {
		t.Errorf("token leaked into logs: %s", e.logs.String())
	}
}

func TestPersonWSHappyPathStepsMatchFixture(t *testing.T) {
	var gotURL string
	var gotDeep bool
	e := newPWSEnv(t, func(ctx context.Context, d personintel.Deps, u string, deep bool, n func(string, map[string]any)) (personintel.Profile, error) {
		gotURL, gotDeep = u, deep
		n("parsed", map[string]any{"username": "jane-doe"})
		n("searching", map[string]any{"message": "Running <b>web</b> searches..."})
		n("search_progress", map[string]any{"category": "linkedin", "count": 3})
		return fakeProfile(u), nil
	}, nil)
	c := e.mustDial(t, "admin")
	sendText(t, c, `{"linkedin_url":"https://www.linkedin.com/in/jane-doe","deep":true}`)
	frames, code := readAll(t, c, 5*time.Second)
	if code != websocket.StatusNormalClosure {
		t.Errorf("close code = %d, want 1000", code)
	}
	if gotURL != "https://www.linkedin.com/in/jane-doe" || !gotDeep {
		t.Errorf("enrich got %q deep=%v", gotURL, gotDeep)
	}
	want := []string{
		`{"step":"parsed","username":"jane-doe"}`,
		`{"step":"searching","message":"Running <b>web</b> searches..."}`,
		`{"step":"search_progress","category":"linkedin","count":3}`,
	}
	if len(frames) != 4 {
		t.Fatalf("frames = %v", frames)
	}
	for i, w := range want {
		if frames[i] != w {
			t.Errorf("frame %d = %s, want %s", i, frames[i], w)
		}
	}
	p := fakeProfile("https://www.linkedin.com/in/jane-doe")
	pb, _ := p.JSON()
	if wantSaved := `{"step":"saved","id":1,"profile":` + string(pb) + `}`; frames[3] != wantSaved {
		t.Errorf("saved frame\n got %s\nwant %s", frames[3], wantSaved)
	}
	if !strings.Contains(frames[3], "Jane <Doe> & Co") {
		t.Errorf("saved frame HTML-escaped: %s", frames[3])
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM person_intel`).Scan(&n); err != nil || n != 1 {
		t.Errorf("rows = %d, %v", n, err)
	}
}

func TestPersonWSRealPipelineStepOrder(t *testing.T) {
	e := newPWSEnv(t, nil, nil)
	c := e.mustDial(t, "admin")
	sendText(t, c, pwsURL)
	frames, _ := readAll(t, c, 30*time.Second)
	var steps []string
	for _, f := range frames {
		var m struct{ Step string }
		_ = json.Unmarshal([]byte(f), &m)
		steps = append(steps, m.Step)
	}
	if len(steps) < 5 || steps[0] != "parsed" || steps[len(steps)-1] != "saved" || steps[len(steps)-2] != "completed" {
		t.Errorf("steps = %v\nlogs: %s\nlast: %s", steps, e.logs.String(), frames[len(frames)-1])
	}
}

func TestPersonWSMissingURL(t *testing.T) {
	e := newPWSEnv(t, func(context.Context, personintel.Deps, string, bool, func(string, map[string]any)) (personintel.Profile, error) {
		t.Error("enrich must not run")
		return personintel.Profile{}, nil
	}, nil)
	for _, body := range []string{`{}`, `{"deep":true}`, `{"linkedin_url":""}`, `{"linkedin_url":null}`} {
		c := e.mustDial(t, "admin")
		sendText(t, c, body)
		frames, code := readAll(t, c, 3*time.Second)
		if len(frames) != 1 || frames[0] != `{"step":"error","message":"No LinkedIn URL provided"}` || code != websocket.StatusNormalClosure {
			t.Errorf("%s: frames=%v code=%d", body, frames, code)
		}
	}
}

func TestPersonWSInvalidURL(t *testing.T) {
	e := newPWSEnv(t, nil, nil)
	c := e.mustDial(t, "admin")
	sendText(t, c, `{"linkedin_url":"https://example.com/in/x"}`)
	frames, code := readAll(t, c, 3*time.Second)
	want := `{"step":"error","message":"Invalid LinkedIn URL. Expected format: https://www.linkedin.com/in/username"}`
	if len(frames) != 1 || frames[0] != want || code != websocket.StatusNormalClosure {
		t.Errorf("frames=%v code=%d", frames, code)
	}
}

func TestPersonWSMalformedFirstFrameGenericError(t *testing.T) {
	e := newPWSEnv(t, nil, nil)
	for _, body := range []string{`not json`, `[1]`, `"x"`, `{"linkedin_url":5}`} {
		c := e.mustDial(t, "admin")
		sendText(t, c, body)
		frames, _ := readAll(t, c, 3*time.Second)
		if len(frames) != 1 || frames[0] != `{"step":"error","message":"Enrichment failed"}` {
			t.Errorf("%s: frames=%v", body, frames)
		}
	}
}

func TestPersonWSOversizedFrameClosed(t *testing.T) {
	e := newPWSEnv(t, nil, func(d *Deps) { d.WSMaxMessageBytes = 128 })
	c := e.mustDial(t, "admin")
	sendText(t, c, `{"linkedin_url":"https://www.linkedin.com/in/`+strings.Repeat("a", 300)+`"}`)
	_, code := readAll(t, c, 3*time.Second)
	if code != websocket.StatusMessageTooBig {
		t.Errorf("close = %d, want 1009", code)
	}
}

func TestPersonWSFirstFrameTimeout(t *testing.T) {
	old := personWSFirstFrameTimeout
	personWSFirstFrameTimeout = 200 * time.Millisecond
	defer func() { personWSFirstFrameTimeout = old }()
	e := newPWSEnv(t, nil, nil)
	c := e.mustDial(t, "admin")
	_, code := readAll(t, c, 3*time.Second)
	if code != websocket.StatusPolicyViolation {
		t.Errorf("close = %d, want 1008", code)
	}
}

func waitGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for goroutines() > base+2 {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines %d, baseline %d: leak", goroutines(), base)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestPersonWSDisconnectCancelsPipeline(t *testing.T) {
	started := make(chan struct{})
	var cancelled atomic.Bool
	e := newPWSEnv(t, func(ctx context.Context, d personintel.Deps, u string, deep bool, n func(string, map[string]any)) (personintel.Profile, error) {
		n("parsed", map[string]any{"username": "x"})
		close(started)
		<-ctx.Done()
		cancelled.Store(true)
		return personintel.Profile{}, ctx.Err()
	}, nil)
	warm := e.mustDial(t, "admin")
	_ = warm.Close(websocket.StatusNormalClosure, "")
	time.Sleep(300 * time.Millisecond)
	base := goroutines()
	c := e.mustDial(t, "admin")
	sendText(t, c, pwsURL)
	<-started
	_ = c.CloseNow()
	deadline := time.Now().Add(5 * time.Second)
	for !cancelled.Load() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !cancelled.Load() {
		t.Fatal("pipeline context not cancelled on disconnect")
	}
	waitGoroutines(t, base)
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM person_intel`).Scan(&n)
	if n != 0 {
		t.Errorf("cancelled run saved %d rows", n)
	}
	// the admin slot is free again
	c2 := e.mustDial(t, "admin")
	sendText(t, c2, `{}`)
	if fr, _ := readAll(t, c2, 3*time.Second); len(fr) != 1 || strings.Contains(fr[0], "already running") {
		t.Errorf("slot not released: %v", fr)
	}
}

func TestPersonWSSecondConcurrentRunRejected(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	e := newPWSEnv(t, func(ctx context.Context, d personintel.Deps, u string, deep bool, n func(string, map[string]any)) (personintel.Profile, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return fakeProfile(u), nil
	}, nil)
	c1 := e.mustDial(t, "admin")
	sendText(t, c1, pwsURL)
	<-started
	c2 := e.mustDial(t, "admin")
	sendText(t, c2, pwsURL)
	fr, code := readAll(t, c2, 3*time.Second)
	if len(fr) != 1 || fr[0] != `{"step":"error","message":"Enrichment already running"}` || code != websocket.StatusNormalClosure {
		t.Errorf("second: %v code=%d", fr, code)
	}
	// a different admin is not blocked by the first
	if e.w.running(identityID(t, e, "admin2")) {
		t.Error("admin2 marked running")
	}
	close(release)
	fr1, _ := readAll(t, c1, 5*time.Second)
	if len(fr1) != 1 || !strings.HasPrefix(fr1[0], `{"step":"saved"`) {
		t.Errorf("first: %v", fr1)
	}
}

func identityID(t *testing.T, e *pwsEnv, user string) int32 {
	t.Helper()
	return e.mustUserByName(t, pwsTag+user).ID
}

func TestPersonWSDeadline(t *testing.T) {
	old := personWSDeadline
	personWSDeadline = 300 * time.Millisecond
	defer func() { personWSDeadline = old }()
	if old != 120*time.Second {
		t.Fatalf("default deadline = %v, want 120s", old)
	}
	e := newPWSEnv(t, func(ctx context.Context, d personintel.Deps, u string, deep bool, n func(string, map[string]any)) (personintel.Profile, error) {
		<-ctx.Done()
		return personintel.Profile{}, ctx.Err()
	}, nil)
	c := e.mustDial(t, "admin")
	sendText(t, c, pwsURL)
	fr, code := readAll(t, c, 5*time.Second)
	if len(fr) != 1 || fr[0] != `{"step":"error","message":"Enrichment timed out"}` || code != websocket.StatusNormalClosure {
		t.Errorf("frames=%v code=%d", fr, code)
	}
}

func TestPersonWSNoInternalErrorTextLeaked(t *testing.T) {
	secret := "postgres://user:hunter2@10.0.0.5/db"
	e := newPWSEnv(t, nil, nil)
	for name, fn := range map[string]pwsEnrich{
		"pipeline error step": func(ctx context.Context, d personintel.Deps, u string, deep bool, n func(string, map[string]any)) (personintel.Profile, error) {
			n("error", map[string]any{"message": "dial " + secret})
			p := fakeProfile(u)
			p.Status = "failed"
			return p, nil
		},
		"returned error": func(ctx context.Context, d personintel.Deps, u string, deep bool, n func(string, map[string]any)) (personintel.Profile, error) {
			return personintel.Profile{}, io.ErrUnexpectedEOF
		},
		"panic": func(ctx context.Context, d personintel.Deps, u string, deep bool, n func(string, map[string]any)) (personintel.Profile, error) {
			panic("boom " + secret)
		},
	} {
		e.w.enrich = fn
		c := e.mustDial(t, "admin")
		sendText(t, c, pwsURL)
		fr, _ := readAll(t, c, 3*time.Second)
		all := strings.Join(fr, "\n")
		if strings.Contains(all, "hunter2") || strings.Contains(all, "boom") || strings.Contains(all, "unexpected EOF") {
			t.Errorf("%s: leaked: %s", name, all)
		}
		if !strings.Contains(all, `{"step":"error","message":"Enrichment failed"}`) {
			t.Errorf("%s: no generic error frame: %s", name, all)
		}
		if strings.Contains(e.logs.String(), "hunter2") {
			t.Errorf("%s: secret in logs", name)
		}
	}
}

func TestPersonWSUnparseableUsernameKeepsPythonMessage(t *testing.T) {
	e := newPWSEnv(t, nil, nil)
	c := e.mustDial(t, "admin")
	sendText(t, c, `{"linkedin_url":"https://linkedin.com/in/"}`)
	fr, _ := readAll(t, c, 5*time.Second)
	if len(fr) < 1 || !strings.HasPrefix(fr[0], `{"step":"error","message":"`) || strings.Contains(fr[0], "Enrichment failed") {
		t.Errorf("frames=%v", fr)
	}
}

func TestPersonWSShutdownClosesGoingAway(t *testing.T) {
	sd, stop := context.WithCancel(context.Background())
	started := make(chan struct{})
	e := newPWSEnv(t, func(ctx context.Context, d personintel.Deps, u string, deep bool, n func(string, map[string]any)) (personintel.Profile, error) {
		close(started)
		<-ctx.Done()
		return personintel.Profile{}, ctx.Err()
	}, func(d *Deps) { d.ShutdownCtx = sd })
	base := goroutines()
	c := e.mustDial(t, "admin")
	sendText(t, c, pwsURL)
	<-started
	stop()
	_, code := readAll(t, c, 5*time.Second)
	if code != websocket.StatusGoingAway {
		t.Errorf("close = %d, want 1001", code)
	}
	waitGoroutines(t, base)
}

func TestPersonWSPerUserConnectionCap(t *testing.T) {
	e := newPWSEnv(t, nil, func(d *Deps) { d.SSEMaxPerUser = 2 })
	e.mustDial(t, "admin")
	e.mustDial(t, "admin")
	_, resp, err := e.dial("token=" + e.token(t, "admin"))
	if err == nil || handshakeStatus(resp) != http.StatusTooManyRequests {
		t.Errorf("err=%v status=%d, want 429", err, handshakeStatus(resp))
	}
	e.mustDial(t, "admin2") // another admin has its own budget
}

func TestPersonWSDeepScrapePassesCallerWorkspace(t *testing.T) {
	var gotWS string
	e := newPWSEnv(t, func(ctx context.Context, d personintel.Deps, u string, deep bool, n func(string, map[string]any)) (personintel.Profile, error) {
		gotWS = d.WorkspaceID
		return fakeProfile(u), nil
	}, nil)
	admin := e.mustUserByName(t, pwsTag+"admin").ID
	ws := pwsTag + "ws-deep"
	e.workspace(t, ws, admin)
	e.member(t, ws, admin, "owner")
	c := e.mustDial(t, "admin")
	sendText(t, c, `{"linkedin_url":"https://www.linkedin.com/in/jane-doe","deep":true}`)
	if fr, _ := readAll(t, c, 5*time.Second); len(fr) != 1 {
		t.Fatalf("frames = %v", fr)
	}
	if gotWS != ws {
		t.Errorf("pipeline workspace = %q, want %q", gotWS, ws)
	}
}

func TestPersonWSWorkspaceHeaderAndForeignFallback(t *testing.T) {
	var gotWS string
	e := newPWSEnv(t, func(ctx context.Context, d personintel.Deps, u string, deep bool, n func(string, map[string]any)) (personintel.Profile, error) {
		gotWS = d.WorkspaceID
		return fakeProfile(u), nil
	}, nil)
	admin := e.mustUserByName(t, pwsTag+"admin").ID
	other := e.mustUserByName(t, pwsTag+"admin2").ID
	mine, foreign := pwsTag+"ws-mine", pwsTag+"ws-foreign"
	e.workspace(t, mine, admin)
	e.member(t, mine, admin, "owner")
	e.workspace(t, foreign, other)
	e.member(t, foreign, other, "owner")
	for hdr, want := range map[string]string{mine: mine, foreign: mine} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.srv.URL, "http")+"/ws/person/enrich?token="+e.token(t, "admin"),
			&websocket.DialOptions{HTTPHeader: http.Header{"X-Workspace-Id": {hdr}}})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		sendText(t, c, pwsURL)
		readAll(t, c, 5*time.Second)
		_ = c.CloseNow()
		if gotWS != want {
			t.Errorf("header %q: workspace %q, want %q", hdr, gotWS, want)
		}
	}
}
