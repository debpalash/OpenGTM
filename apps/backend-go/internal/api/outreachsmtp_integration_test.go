//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachsend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ratelimit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/smtpclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testsmtp"
)

// osmtpEnvVars are the process-environment values the test resolver sees (the platform's global SMTP settings).
var osmtpEnvVars map[string]string

const osmtpPassword = "fake-password-not-real"

type osmtpEnv struct {
	oseqEnv
	res  *secrets.Resolver
	logs *bytes.Buffer
	t    *testing.T
	dial atomic.Int32 // dial attempts made by the test mailer
}

func osmtpPurge() {
	for _, q := range []string{`DELETE FROM workspace_settings WHERE workspace_id IN ('ws-alpha','ws-beta')`} {
		wcrudOwnerExecQuiet(q)
	}
}

// newOsmtpEnv serves only the SMTP routes. fake, when set, becomes the mailer's server (TLS off allowed for 127.0.0.1).
func newOsmtpEnv(t *testing.T, fake *testsmtp.Server, limiter *ratelimit.Limiter) *osmtpEnv {
	t.Helper()
	e := newLeadsEnv(t)
	e.seed(t, "erin", pythonHash, true, "user", false)
	e.member(t, "ws-alpha", e.mustUserByName(t, "erin").ID, "editor")
	osmtpPurge()
	t.Cleanup(osmtpPurge)
	env, err := secrets.NewEnvelope(secrets.Config{MasterKey: "osmtp-test-key", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	osmtpEnvVars = nil
	t.Cleanup(func() { osmtpEnvVars = nil })
	get := func(k string) string { return osmtpEnvVars[k] }
	out := &osmtpEnv{t: t, logs: &bytes.Buffer{}}
	d := Deps{Pool: e.pool, Signer: testSigner, Identity: e.st, Envelope: env, Getenv: get, Limiter: limiter,
		Log:    slog.New(slog.NewTextHandler(out.logs, nil)),
		Config: config.APIConfig{AppEnv: "dev"}, Flags: config.Flags{PGLeadStore: true, Outreach: config.OutreachSettings{GoRoutesEnabled: true}}}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(e.pool, nil))
	registerOutreachSMTP(r, d)
	e.h = r.Handler()
	out.oseqEnv = oseqEnv{e}
	out.res = secrets.NewResolver(env, secrets.NewPgStore(e.pool), get)
	prev := smtpMailerFor
	t.Cleanup(func() { smtpMailerFor = prev })
	if fake != nil {
		smtpMailerFor = func(Deps) outreachsend.Mailer {
			return &outreachsend.SMTPMailer{
				Client: &smtpclient.Client{RootCAs: fake.CertPool(), PlainHosts: []string{"127.0.0.1"}},
				Dial: egress.DialContext(func(ctx context.Context, network, _ string) (net.Conn, error) {
					out.dial.Add(1)
					var nd net.Dialer
					return nd.DialContext(ctx, network, fake.Addr())
				}),
			}
		}
	}
	return out
}

func (e *osmtpEnv) set(ws string, kv map[string]string) {
	e.t.Helper()
	for k, v := range kv {
		if err := e.res.Set(context.Background(), ws, k, v); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *osmtpEnv) configure(ws, host, port, tls string) {
	e.set(ws, map[string]string{"SMTP_HOST": host, "SMTP_PORT": port, "SMTP_EMAIL": "sender@contract.example.com",
		"SMTP_PASSWORD": osmtpPassword, "SMTP_USE_TLS": tls, "SMTP_FROM_NAME": "Contract Sender"})
}

func osmtpActor(name string) (who, ws string) {
	switch {
	case strings.HasSuffix(name, "_no_auth_401"), strings.HasSuffix(name, "_no_auth"):
		return "", ""
	case strings.HasSuffix(name, "_as_editor_403"):
		return "erin", ""
	case strings.HasSuffix(name, "_beta_bob"), name == "smtp_status_beta_still_unconfigured":
		return "bob", "ws-beta"
	}
	return "alice", ""
}

var osmtpOrder = []string{
	"smtp_status_no_auth", "smtp_config_no_auth_401", "smtp_config_as_editor_403", "smtp_test_as_editor_403",
	"smtp_status_unconfigured", "smtp_test_unconfigured_400", "smtp_config_none", "smtp_status_after_none",
	"smtp_config_bad_type_422", "smtp_config_host", "smtp_config_email", "smtp_status_partial", "smtp_config_password",
	"smtp_config_port", "smtp_config_use_tls_false", "smtp_config_max_per_hour", "smtp_config_from_name", "smtp_status_configured",
	"smtp_test_bad_body_422", "smtp_test_invalid_recipient_400", "smtp_test_crlf_recipient_400", "smtp_test_ok",
	"smtp_status_beta_bob", "smtp_status_beta_still_unconfigured",
}

func TestStatusShapeMatchesFixtures(t *testing.T) {
	// The recorded chain configures host 127.0.0.1 port 25251 without TLS: the fake server stands in for it.
	srv := testsmtp.New(t, testsmtp.Options{NoSTARTTLS: true, AuthUser: "sender@contract.example.com", AuthPass: osmtpPassword})
	e := newOsmtpEnv(t, srv, nil)
	for _, name := range osmtpOrder {
		fx := contract.Load(t, "outreach/"+name)
		who, ws := osmtpActor(name)
		body := ""
		if b := strings.TrimSpace(string(fx.Request.Body)); b != "" && b != "null" {
			body = b
		}
		rec := e.call(who, ws, fx.Request.Method, fx.Request.Path, body)
		var got, want any
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		_ = json.Unmarshal(fx.Response.Body, &want)
		if rec.Code != fx.Response.Status || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %d %s want %d %s", name, rec.Code, rec.Body.String(), fx.Response.Status, fx.Response.Body)
		}
	}
	if ms := srv.Messages(); len(ms) != 1 {
		t.Fatalf("fake server got %d messages, want 1", len(ms))
	}
}

func TestStatusNeverReturnsPassword(t *testing.T) {
	srv := testsmtp.New(t, testsmtp.Options{NoSTARTTLS: true, AuthUser: "sender@contract.example.com", AuthPass: osmtpPassword})
	e := newOsmtpEnv(t, srv, nil)
	e.configure("ws-alpha", "127.0.0.1", "25251", "0")
	var all strings.Builder
	for _, c := range [][3]string{
		{"GET", "/api/outreach/smtp/status", ""},
		{"PUT", "/api/outreach/smtp/config", `{"smtp_password":"` + osmtpPassword + `"}`},
		{"POST", "/api/outreach/smtp/test", `{"to_email":"test@example.com"}`},
		{"PUT", "/api/outreach/smtp/config", `{"smtp_port":"abc","smtp_password":"` + osmtpPassword + `"}`},
	} {
		rec := e.call("alice", "", c[0], c[1], c[2])
		all.WriteString(rec.Body.String())
		all.WriteString(fmt.Sprint(rec.Header()))
	}
	if strings.Contains(all.String(), osmtpPassword) || strings.Contains(e.logs.String(), osmtpPassword) {
		t.Fatalf("password leaked: %s | %s", all.String(), e.logs.String())
	}
	var raw string // stored encrypted, not plaintext
	if err := wcrudOwnerQueryRow(t, `SELECT value FROM workspace_settings WHERE workspace_id='ws-alpha' AND key='SMTP_PASSWORD'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "" || strings.Contains(raw, osmtpPassword) {
		t.Fatalf("password not stored through the envelope: %q", raw)
	}
}

func TestConfigWritesOnlyGivenFields(t *testing.T) {
	e := newOsmtpEnv(t, nil, nil)
	e.configure("ws-alpha", "smtp.old.example", "2525", "1")
	code, _, body := e.as("alice", "", "PUT", "/api/outreach/smtp/config", `{"smtp_host":"smtp.new.example","smtp_max_per_hour":12,"smtp_email":null,"smtp_from_name":"  "}`)
	if code != 200 || !strings.Contains(body, `"ok"`) {
		t.Fatalf("%d %s", code, body)
	}
	_, got, _ := e.as("alice", "", "GET", "/api/outreach/smtp/status", "")
	want := map[string]any{"configured": true, "host": "smtp.new.example", "email": "sender@contract.example.com", "from_name": "Contract Sender", "max_per_hour": float64(12)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("status = %v", got)
	}
	var port, tls, pw string
	for k, p := range map[string]*string{"SMTP_PORT": &port, "SMTP_USE_TLS": &tls} {
		v, _, err := e.res.Get(context.Background(), "ws-alpha", k)
		if err != nil {
			t.Fatal(err)
		}
		*p = v
	}
	pw, _, _ = e.res.Get(context.Background(), "ws-alpha", "SMTP_PASSWORD")
	if port != "2525" || tls != "1" || pw != osmtpPassword {
		t.Fatalf("untouched fields changed: %q %q", port, tls)
	}
	// booleans and ints are stored as Python does
	e.as("alice", "", "PUT", "/api/outreach/smtp/config", `{"smtp_port":"465","smtp_use_tls":false}`)
	port, _, _ = e.res.Get(context.Background(), "ws-alpha", "SMTP_PORT")
	tls, _, _ = e.res.Get(context.Background(), "ws-alpha", "SMTP_USE_TLS")
	if port != "465" || tls != "0" {
		t.Fatalf("port=%q tls=%q", port, tls)
	}
}

func TestConfigRequiresAdmin(t *testing.T) {
	e := newOsmtpEnv(t, nil, nil)
	for _, c := range []struct {
		who, ws string
		code    int
	}{{"erin", "", 403}, {"bob", "ws-alpha", 403}, {"", "", 401}, {"alice", "", 200}} {
		if code, _, body := e.as(c.who, c.ws, "PUT", "/api/outreach/smtp/config", `{"smtp_max_per_hour":5}`); code != c.code {
			t.Errorf("%s/%s: %d %s", c.who, c.ws, code, body)
		}
		if code, _, body := e.as(c.who, c.ws, "POST", "/api/outreach/smtp/test", `{"to_email":"a@b.example"}`); (code == 200) || (c.code == 403 && code != 403) || (c.code == 401 && code != 401) {
			t.Errorf("test %s/%s: %d %s", c.who, c.ws, code, body)
		}
	}
	// reads need only membership: the viewer sees the status
	if code, _, body := e.as("bob", "ws-alpha", "GET", "/api/outreach/smtp/status", ""); code != 200 {
		t.Errorf("viewer status: %d %s", code, body)
	}
}

func TestConfigHostValidation422(t *testing.T) {
	e := newOsmtpEnv(t, nil, nil)
	for _, h := range []string{"https://smtp.example.com", "smtp.example.com/path", "smtp example.com", "user@smtp.example.com", "smtp.example.com:587", "a\tb", "smtp.example.com\r\nX: y", "-bad.example.com", strings.Repeat("a", 254)} {
		b, _ := json.Marshal(map[string]any{"smtp_host": h})
		code, m, body := e.as("alice", "", "PUT", "/api/outreach/smtp/config", string(b))
		if code != 422 {
			t.Errorf("host %q: %d %s", h, code, body)
			continue
		}
		d := m["detail"].([]any)[0].(map[string]any)
		if d["type"] != "value_error" || !reflect.DeepEqual(d["loc"], []any{"body", "smtp_host"}) {
			t.Errorf("host %q: shape %s", h, body)
		}
	}
	for _, h := range []string{"smtp.example.com", "127.0.0.1", "mail-1.example.co.uk", "2001:db8::1", " smtp.example.com ", "localhost"} {
		b, _ := json.Marshal(map[string]any{"smtp_host": h})
		if code, _, body := e.as("alice", "", "PUT", "/api/outreach/smtp/config", string(b)); code != 200 {
			t.Errorf("host %q: %d %s", h, code, body)
		}
	}
	// blank is ignored like set_secret, and nothing was stored by the rejected requests
	if code, _, _ := e.as("alice", "", "PUT", "/api/outreach/smtp/config", `{"smtp_host":"   "}`); code != 200 {
		t.Errorf("blank host: %d", code)
	}
	if code, _, _ := e.as("alice", "", "PUT", "/api/outreach/smtp/config", `{"smtp_host":42}`); code != 422 {
		t.Errorf("numeric host: %d", code)
	}
}

func TestConfigPortRange422(t *testing.T) {
	e := newOsmtpEnv(t, nil, nil)
	for _, c := range []struct {
		body string
		code int
	}{
		{`{"smtp_port":0}`, 422}, {`{"smtp_port":-1}`, 422}, {`{"smtp_port":65536}`, 422}, {`{"smtp_port":1}`, 200}, {`{"smtp_port":65535}`, 200},
		{`{"smtp_max_per_hour":0}`, 422}, {`{"smtp_max_per_hour":100001}`, 422}, {`{"smtp_max_per_hour":1}`, 200}, {`{"smtp_max_per_hour":100000}`, 200},
		{`{"smtp_port":"abc"}`, 422}, {`{"smtp_use_tls":"maybe"}`, 422}, {`{"smtp_password":"` + strings.Repeat("x", 2000) + `"}`, 422},
		{`[1]`, 422}, {`{"smtp_email":"a\u0000b"}`, 422},
	} {
		code, m, body := e.as("alice", "", "PUT", "/api/outreach/smtp/config", c.body)
		if code != c.code {
			t.Errorf("%.60s: %d %s", c.body, code, body)
			continue
		}
		if code == 422 {
			if d, _ := m["detail"].([]any); len(d) == 0 {
				t.Errorf("%.60s: no detail", c.body)
			}
		}
	}
	// an out-of-range value must not be stored
	e.as("alice", "", "PUT", "/api/outreach/smtp/config", `{"smtp_port":70000,"smtp_max_per_hour":9}`)
	_, got, _ := e.as("alice", "", "GET", "/api/outreach/smtp/status", "")
	if got["max_per_hour"] == float64(9) {
		t.Fatalf("a rejected request wrote a field: %v", got)
	}
}

func TestSMTPTestSendsViaFakeServer(t *testing.T) {
	srv := testsmtp.New(t, testsmtp.Options{AuthUser: "sender@contract.example.com", AuthPass: osmtpPassword})
	e := newOsmtpEnv(t, srv, nil)
	e.set("ws-alpha", map[string]string{"SMTP_HOST": "localhost", "SMTP_PORT": "587", "SMTP_EMAIL": "sender@contract.example.com",
		"SMTP_PASSWORD": osmtpPassword, "SMTP_FROM_NAME": "Contract Sender"})
	code, m, body := e.as("alice", "", "POST", "/api/outreach/smtp/test", `{"to_email":"dest@example.com"}`)
	if code != 200 || m["status"] != "ok" || m["message"] != "Test email sent to dest@example.com" {
		t.Fatalf("%d %s", code, body)
	}
	ms := srv.Messages()
	if len(ms) != 1 || ms[0].From != "sender@contract.example.com" || !reflect.DeepEqual(ms[0].To, []string{"dest@example.com"}) || !ms[0].TLS {
		t.Fatalf("messages %+v", ms)
	}
	data := string(ms[0].Data)
	if !strings.Contains(data, "Subject: =?utf-8?") && !strings.Contains(data, "Subject: OpenGTM") {
		t.Fatalf("subject missing: %s", data)
	}
	if !strings.Contains(data, "Your SMTP configuration is working.") || !strings.Contains(data, "Contract Sender") {
		t.Fatalf("body/from missing: %s", data)
	}
}

func TestSMTPTestUnconfigured400(t *testing.T) {
	srv := testsmtp.New(t, testsmtp.Options{})
	e := newOsmtpEnv(t, srv, nil)
	e.set("ws-alpha", map[string]string{"SMTP_HOST": "localhost", "SMTP_EMAIL": "sender@contract.example.com"}) // no password
	code, m, body := e.as("alice", "", "POST", "/api/outreach/smtp/test", `{"to_email":"dest@example.com"}`)
	if code != 400 || m["detail"] != "SMTP not configured" || e.dial.Load() != 0 {
		t.Fatalf("%d %s dials=%d", code, body, e.dial.Load())
	}
}

func TestSMTPTestInvalidRecipient400(t *testing.T) {
	srv := testsmtp.New(t, testsmtp.Options{})
	e := newOsmtpEnv(t, srv, nil)
	e.configure("ws-alpha", "localhost", "587", "1")
	for _, to := range []string{"not an address", "a@example.com\r\nBcc: b@example.com", "", "a@b", "a@b.c,d@e.f"} {
		b, _ := json.Marshal(map[string]any{"to_email": to})
		code, m, body := e.as("alice", "", "POST", "/api/outreach/smtp/test", string(b))
		if code != 400 || m["detail"] != "invalid recipient address" {
			t.Errorf("%q: %d %s", to, code, body)
		}
	}
	if e.dial.Load() != 0 || len(srv.Messages()) != 0 {
		t.Fatalf("an invalid recipient reached the network: dials=%d", e.dial.Load())
	}
}

func TestSMTPTestBlockedHostFailsClosed(t *testing.T) {
	// The production mailer: "localhost" resolves to loopback, which the egress guard refuses.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_, _ = c.Write([]byte("220 banner " + osmtpPassword + "\r\n"))
			c.Close()
		}
	}()
	e := newOsmtpEnv(t, nil, nil) // smtpMailerFor untouched: production wiring
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	e.configure("ws-alpha", "localhost", port, "1")
	code, m, body := e.as("alice", "", "POST", "/api/outreach/smtp/test", `{"to_email":"dest@example.com"}`)
	time.Sleep(100 * time.Millisecond)
	if code != 400 || accepted.Load() != 0 {
		t.Fatalf("%d %s accepted=%d", code, body, accepted.Load())
	}
	if d, _ := m["detail"].(string); d == "" || strings.Contains(body, osmtpPassword) {
		t.Fatalf("detail %q", body)
	}
}

func TestSMTPTestErrorTextNeverLeaksPassword(t *testing.T) {
	srv := testsmtp.New(t, testsmtp.Options{AuthUser: "sender@contract.example.com", AuthPass: "other-password",
		AuthFailReply: "535 5.7.8 bad credentials for " + osmtpPassword + " (" + "ZmFrZS1wYXNzd29yZC1ub3QtcmVhbA==" + ")"})
	e := newOsmtpEnv(t, srv, nil)
	e.configure("ws-alpha", "localhost", "587", "1")
	code, m, body := e.as("alice", "", "POST", "/api/outreach/smtp/test", `{"to_email":"dest@example.com"}`)
	if code != 400 || strings.Contains(body, osmtpPassword) || strings.Contains(body, "ZmFrZS1w") || strings.Contains(e.logs.String(), osmtpPassword) {
		t.Fatalf("%d %s", code, body)
	}
	if d, _ := m["detail"].(string); !strings.HasPrefix(d, "535 ") {
		t.Fatalf("server reply text missing: %q", d)
	}
}

func TestSMTPTestRateLimited(t *testing.T) {
	srv := testsmtp.New(t, testsmtp.Options{})
	e := newOsmtpEnv(t, srv, frozenLimiter())
	for i := 0; i < 10; i++ {
		if code, _, body := e.as("alice", "ws-alpha", "POST", "/api/outreach/smtp/test", `{"to_email":"nope"}`); code != 400 {
			t.Fatalf("request %d: %d %s", i, code, body)
		}
	}
	code, m, body := e.as("alice", "ws-alpha", "POST", "/api/outreach/smtp/test", `{"to_email":"nope"}`)
	if code != 429 || !strings.Contains(fmt.Sprint(m["error"]), "Rate limit exceeded") {
		t.Fatalf("11th: %d %s", code, body)
	}
	// another workspace has its own budget; reads are not limited
	if code, _, body := e.as("bob", "ws-beta", "POST", "/api/outreach/smtp/test", `{"to_email":"nope"}`); code == 429 {
		t.Fatalf("ws-beta shares the budget: %d %s", code, body)
	}
	if code, _, _ := e.as("alice", "ws-alpha", "GET", "/api/outreach/smtp/status", ""); code != 200 {
		t.Fatalf("status limited: %d", code)
	}
}

func TestConfigTenantIsolation(t *testing.T) {
	e := newOsmtpEnv(t, nil, nil)
	if code, _, body := e.as("alice", "ws-alpha", "PUT", "/api/outreach/smtp/config",
		`{"smtp_host":"smtp.alpha.example","smtp_email":"a@alpha.example","smtp_password":"`+osmtpPassword+`","smtp_max_per_hour":9}`); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	_, beta, _ := e.as("bob", "ws-beta", "GET", "/api/outreach/smtp/status", "")
	want := map[string]any{"configured": false, "host": nil, "email": nil, "from_name": "OpenGTM", "max_per_hour": float64(50)}
	if !reflect.DeepEqual(beta, want) {
		t.Fatalf("ws-beta saw ws-alpha config: %v", beta)
	}
	// bob (admin of ws-beta only) writes ws-beta; ws-alpha stays untouched
	if code, _, body := e.as("bob", "ws-beta", "PUT", "/api/outreach/smtp/config", `{"smtp_host":"smtp.beta.example"}`); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	_, alpha, _ := e.as("alice", "ws-alpha", "GET", "/api/outreach/smtp/status", "")
	if alpha["host"] != "smtp.alpha.example" || alpha["max_per_hour"] != float64(9) {
		t.Fatalf("ws-alpha changed: %v", alpha)
	}
	// a foreign workspace id is refused outright
	if code, _, _ := e.as("bob", "ws-alpha-nope", "PUT", "/api/outreach/smtp/config", `{"smtp_host":"x.example"}`); code != 403 && code != 404 {
		t.Fatalf("foreign workspace: %d", code)
	}
}

// Plan 09 final security H1: a workspace admin who points smtp_host at their own server must not
// have the platform's global SMTP username and password sent there.
func TestSMTPTestNeverSendsGlobalPasswordToWorkspaceHost(t *testing.T) {
	srv := testsmtp.New(t, testsmtp.Options{AuthUser: "platform@example.com", AuthPass: "platform-secret-pw"})
	e := newOsmtpEnv(t, srv, nil)
	osmtpEnvVars = map[string]string{"SMTP_HOST": "smtp.platform.example", "SMTP_EMAIL": "platform@example.com", "SMTP_PASSWORD": "platform-secret-pw"}
	code, m, body := e.as("alice", "", "PUT", "/api/outreach/smtp/config", `{"smtp_host":"mx.attacker.example","smtp_port":465}`)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	code, m, body = e.as("alice", "", "GET", "/api/outreach/smtp/status", "")
	if code != 200 || m["configured"] != false {
		t.Fatalf("status %d %s", code, body)
	}
	code, m, body = e.as("alice", "", "POST", "/api/outreach/smtp/test", `{"to_email":"dest@example.com"}`)
	detail, _ := m["detail"].(string)
	if code != 400 || !strings.HasPrefix(detail, "SMTP not configured: ") || e.dial.Load() != 0 || len(srv.Messages()) != 0 {
		t.Fatalf("%d %s dials=%d", code, body, e.dial.Load())
	}
	if strings.Contains(body, "platform-secret-pw") || strings.Contains(body, "platform@example.com") {
		t.Fatalf("platform credentials leaked: %s", body)
	}
	// with the workspace's own credentials it works
	e.set("ws-alpha", map[string]string{"SMTP_HOST": "localhost", "SMTP_PORT": "587", "SMTP_EMAIL": "platform@example.com", "SMTP_PASSWORD": "platform-secret-pw"})
	if code, _, body = e.as("alice", "", "POST", "/api/outreach/smtp/test", `{"to_email":"dest@example.com"}`); code != 200 {
		t.Fatalf("own creds: %d %s", code, body)
	}
}

// Plan 09 final security M2: the bucket is the authenticated workspace, not whatever X-Workspace-Id says. A caller who
// omits the header (active workspace) shares the budget of the same workspace, and a spoofed header is refused, not a fresh bucket.
func TestSMTPTestLimiterKeysOnAuthenticatedWorkspace(t *testing.T) {
	srv := testsmtp.New(t, testsmtp.Options{})
	e := newOsmtpEnv(t, srv, frozenLimiter())
	for i := 0; i < 10; i++ {
		e.as("alice", "ws-alpha", "POST", "/api/outreach/smtp/test", `{"to_email":"nope"}`)
	}
	if code, _, body := e.as("alice", "", "POST", "/api/outreach/smtp/test", `{"to_email":"nope"}`); code != 429 {
		t.Fatalf("header-less request escaped the workspace bucket: %d %s", code, body)
	}
	if code, _, body := e.as("alice", "ws-beta", "POST", "/api/outreach/smtp/test", `{"to_email":"nope"}`); code != 403 {
		t.Fatalf("spoofed workspace header: %d %s", code, body)
	}
}

// S-L15: the operator's global relay host and mailbox are not shown to workspace members.
func TestStatusHidesPlatformHostAndEmail(t *testing.T) {
	e := newOsmtpEnv(t, nil, nil)
	osmtpEnvVars = map[string]string{"SMTP_HOST": "smtp.platform.example", "SMTP_EMAIL": "platform@x.co", "SMTP_PASSWORD": "platform-pw"}
	_, got, body := e.as("alice", "", "GET", "/api/outreach/smtp/status", "")
	if got["configured"] != true || got["host"] != nil || got["email"] != nil || got["source"] != "platform" || strings.Contains(body, "platform.example") || strings.Contains(body, "platform@x.co") {
		t.Fatalf("status = %s", body)
	}
}
