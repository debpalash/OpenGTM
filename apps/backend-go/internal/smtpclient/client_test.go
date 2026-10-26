package smtpclient

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testsmtp"
)

const testPassword = "s3cret-pw-DO-NOT-LEAK"

var testMsg = []byte("From: Me <me@sender.example>\r\nTo: you@dest.example\r\nSubject: hi\r\n" +
	"Message-ID: <abc123@sender.example>\r\nContent-Type: text/plain\r\n\r\nline one\r\n.dot line\r\n")

// toServer dials the test server whatever address the client asks for, so the
// port-driven TLS semantics (587, 465) can be exercised on a random port.
func toServer(srv *testsmtp.Server) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, srv.Addr())
	}
}

func cfgFor(srv *testsmtp.Server, port int, useTLS bool) Config {
	return Config{Host: "localhost", Port: port, Email: "me@sender.example", Password: testPassword, UseTLS: useTLS, Dial: toServer(srv)}
}

func newClient(pool *x509.CertPool, plain ...string) *Client {
	return &Client{RootCAs: pool, PlainHosts: plain}
}

func TestSendDeliversMessage(t *testing.T) {
	srv := testsmtp.New(t, testsmtp.Options{AuthUser: "me@sender.example", AuthPass: testPassword})
	id, err := newClient(srv.CertPool()).Send(context.Background(), cfgFor(srv, 587, true), "me@sender.example", "you@dest.example", testMsg)
	if err != nil {
		t.Fatal(err)
	}
	if id != "<abc123@sender.example>" {
		t.Fatalf("message id = %q", id)
	}
	ms := srv.Messages()
	if len(ms) != 1 || ms[0].From != "me@sender.example" || len(ms[0].To) != 1 || ms[0].To[0] != "you@dest.example" {
		t.Fatalf("messages = %+v", ms)
	}
	if !bytes.Equal(ms[0].Data, testMsg) {
		t.Fatalf("data mismatch (dot stuffing?): %q", ms[0].Data)
	}
	if ms[0].AuthUser != "me@sender.example" {
		t.Fatalf("auth user %q", ms[0].AuthUser)
	}
}

func TestSTARTTLSOn587Semantics(t *testing.T) {
	srv := testsmtp.New(t, testsmtp.Options{})
	if _, err := newClient(srv.CertPool()).Send(context.Background(), cfgFor(srv, 587, true), "me@sender.example", "you@dest.example", testMsg); err != nil {
		t.Fatal(err)
	}
	if ms := srv.Messages(); len(ms) != 1 || !ms[0].TLS {
		t.Fatalf("587 must upgrade with STARTTLS before AUTH/DATA: %+v", ms)
	}
	// 587 with a server that does not offer STARTTLS fails; the password is never sent in clear.
	bare := testsmtp.New(t, testsmtp.Options{NoSTARTTLS: true})
	_, err := newClient(bare.CertPool(), "localhost").Send(context.Background(), cfgFor(bare, 587, true), "me@sender.example", "you@dest.example", testMsg)
	if err == nil || len(bare.Messages()) != 0 {
		t.Fatalf("587 without STARTTLS must fail, err=%v", err)
	}
	// Other port with TLS: STARTTLS when offered.
	other := testsmtp.New(t, testsmtp.Options{})
	if _, err := newClient(other.CertPool()).Send(context.Background(), cfgFor(other, 2525, true), "me@sender.example", "you@dest.example", testMsg); err != nil {
		t.Fatal(err)
	}
	if ms := other.Messages(); len(ms) != 1 || !ms[0].TLS {
		t.Fatalf("2525 with UseTLS must use STARTTLS when offered: %+v", ms)
	}
}

func TestImplicitTLSOn465(t *testing.T) {
	srv := testsmtp.New(t, testsmtp.Options{ImplicitTLS: true})
	if _, err := newClient(srv.CertPool()).Send(context.Background(), cfgFor(srv, 465, true), "me@sender.example", "you@dest.example", testMsg); err != nil {
		t.Fatal(err)
	}
	if ms := srv.Messages(); len(ms) != 1 || !ms[0].TLS {
		t.Fatalf("465 must be implicit TLS: %+v", ms)
	}
}

func TestCertificateVerificationRequired(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts testsmtp.Options
		port int
	}{{"starttls", testsmtp.Options{}, 587}, {"implicit", testsmtp.Options{ImplicitTLS: true}, 465}} {
		srv := testsmtp.New(t, tc.opts)
		// Client does not trust the self-signed certificate.
		_, err := (&Client{}).Send(context.Background(), cfgFor(srv, tc.port, true), "me@sender.example", "you@dest.example", testMsg)
		var ua x509.UnknownAuthorityError
		if err == nil || !errors.As(err, &ua) {
			t.Errorf("%s: err = %v, want unknown authority", tc.name, err)
		}
		if len(srv.Messages()) != 0 {
			t.Errorf("%s: message delivered over unverified TLS", tc.name)
		}
		// Wrong server name is refused too.
		c := cfgFor(srv, tc.port, true)
		c.Host = "mail.wrong.example"
		if _, err := newClient(srv.CertPool()).Send(context.Background(), c, "me@sender.example", "you@dest.example", testMsg); err == nil {
			t.Errorf("%s: hostname mismatch accepted", tc.name)
		}
	}
}

func TestPlainTextRefusedForNonTestHosts(t *testing.T) {
	srv := testsmtp.New(t, testsmtp.Options{NoSTARTTLS: true})
	_, err := newClient(srv.CertPool()).Send(context.Background(), cfgFor(srv, 25, false), "me@sender.example", "you@dest.example", testMsg)
	if err == nil || !strings.Contains(err.Error(), "plaintext") {
		t.Fatalf("err = %v, want plaintext refusal", err)
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Fatal("password in error")
	}
	if len(srv.Messages()) != 0 {
		t.Fatal("delivered in clear")
	}
	// UseTLS but the server offers no STARTTLS on a non-587 port: still refused.
	if _, err := newClient(srv.CertPool()).Send(context.Background(), cfgFor(srv, 2525, true), "me@sender.example", "you@dest.example", testMsg); err == nil {
		t.Fatal("UseTLS without STARTTLS must not fall back to plaintext")
	}
	// Allow-listed test host: plain works.
	if _, err := newClient(srv.CertPool(), "localhost").Send(context.Background(), cfgFor(srv, 25, false), "me@sender.example", "you@dest.example", testMsg); err != nil {
		t.Fatalf("allow-listed plain: %v", err)
	}
	if len(srv.Messages()) != 1 {
		t.Fatal("allow-listed plain not delivered")
	}
}

func TestAuthFailureError(t *testing.T) {
	srv := testsmtp.New(t, testsmtp.Options{AuthUser: "me@sender.example", AuthPass: "other"})
	_, err := newClient(srv.CertPool()).Send(context.Background(), cfgFor(srv, 587, true), "me@sender.example", "you@dest.example", testMsg)
	if err == nil || !strings.Contains(err.Error(), "535 5.7.8 authentication failed") {
		t.Fatalf("err = %v, want verbatim server reply", err)
	}
	// Parity quirk: Python's _is_hard_bounce treats any 5xx token (535 too) as hard.
	if !IsHardBounce(err.Error()) {
		t.Fatal("535 must classify like Python (5xx token)")
	}
}

func TestRecipientRejectedVerbatim(t *testing.T) {
	srv := testsmtp.New(t, testsmtp.Options{RcptReplies: map[string]string{"you@dest.example": "550 5.1.1 <you@dest.example>: user unknown"}})
	_, err := newClient(srv.CertPool()).Send(context.Background(), cfgFor(srv, 587, true), "me@sender.example", "you@dest.example", testMsg)
	if err == nil || !strings.Contains(err.Error(), "550 5.1.1 <you@dest.example>: user unknown") {
		t.Fatalf("err = %v", err)
	}
	if !IsHardBounce(err.Error()) {
		t.Fatal("550 must classify as hard bounce")
	}
}

func TestHardBounceClassificationMatchesVectors(t *testing.T) {
	raw, err := os.ReadFile("../outreachsend/testdata/hard_bounce.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			Error    *string `json:"error"`
			Expected bool    `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) < 30 {
		t.Fatalf("only %d vectors", len(f.Cases))
	}
	for _, c := range f.Cases {
		s := ""
		if c.Error != nil {
			s = *c.Error
		}
		if got := IsHardBounce(s); got != c.Expected {
			t.Errorf("IsHardBounce(%q) = %v, want %v", s, got, c.Expected)
		}
	}
}

func TestTimeoutHonoured(t *testing.T) {
	srv := testsmtp.New(t, testsmtp.Options{Hang: true})
	c := newClient(srv.CertPool())
	c.Timeout = 200 * time.Millisecond
	start := time.Now()
	_, err := c.Send(context.Background(), cfgFor(srv, 465, true), "me@sender.example", "you@dest.example", testMsg)
	if err == nil {
		t.Fatal("want timeout error")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestContextCancelClosesConnection(t *testing.T) {
	srv := testsmtp.New(t, testsmtp.Options{Hang: true})
	ctx, cancel := context.WithCancel(context.Background())
	var conn net.Conn
	cfg := cfgFor(srv, 25, false)
	cfg.Dial = func(ctx context.Context, n, a string) (net.Conn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, n, srv.Addr())
		conn = c
		return c, err
	}
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	done := make(chan error, 1)
	go func() {
		_, err := newClient(srv.CertPool(), "localhost").Send(ctx, cfg, "me@sender.example", "you@dest.example", testMsg)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want error after cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send did not return after cancel")
	}
	if _, err := conn.Write([]byte("x")); err == nil {
		t.Fatal("connection still open after cancel")
	}
}

func TestErrorNeverContainsPassword(t *testing.T) {
	// A server that echoes the credential back in its failure reply, a dial
	// error, and a TLS error: none may surface the password.
	srv := testsmtp.New(t, testsmtp.Options{AuthUser: "x", AuthPass: "y", AuthFailReply: "535 5.7.8 bad credentials for " + testPassword})
	_, err := newClient(srv.CertPool()).Send(context.Background(), cfgFor(srv, 587, true), "me@sender.example", "you@dest.example", testMsg)
	if err == nil || strings.Contains(err.Error(), testPassword) {
		t.Fatalf("echoed reply leaked: %v", err)
	}
	cfg := cfgFor(srv, 587, true)
	cfg.Dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("dial failed for " + testPassword)
	}
	_, err = newClient(srv.CertPool()).Send(context.Background(), cfg, "me@sender.example", "you@dest.example", testMsg)
	if err == nil || strings.Contains(err.Error(), testPassword) {
		t.Fatalf("dial error leaked: %v", err)
	}
	_, err = (&Client{}).Send(context.Background(), cfgFor(srv, 587, true), "me@sender.example", "you@dest.example", testMsg)
	if err == nil || strings.Contains(err.Error(), testPassword) {
		t.Fatalf("tls error leaked: %v", err)
	}
}

func TestEgressBlockedHostSurfaces(t *testing.T) {
	cfg := Config{Host: "127.0.0.1", Port: 587, Email: "me@sender.example", Password: testPassword, UseTLS: true,
		Dial: egress.NewDialContext("production", nil)}
	_, err := (&Client{}).Send(context.Background(), cfg, "me@sender.example", "you@dest.example", testMsg)
	if !errors.Is(err, egress.ErrBlocked) {
		t.Fatalf("err = %v, want egress.ErrBlocked", err)
	}
}

func TestNilDialRefused(t *testing.T) {
	_, err := (&Client{}).Send(context.Background(), Config{Host: "h", Port: 587, Email: "a@b.c", Password: "p", UseTLS: true}, "a@b.c", "x@y.z", testMsg)
	if err == nil {
		t.Fatal("nil Dial must be refused, never fall back to an unguarded dialer")
	}
}

func TestClientGeneratedErrorsNeverHardBounce(t *testing.T) {
	var errs []error
	send := func(c *Client, cfg Config, ctx context.Context) {
		_, err := c.Send(ctx, cfg, "me@sender.example", "you@dest.example", testMsg)
		if err == nil {
			t.Fatal("want error")
		}
		errs = append(errs, err)
	}
	bare := testsmtp.New(t, testsmtp.Options{NoSTARTTLS: true})
	send(newClient(bare.CertPool(), "localhost"), cfgFor(bare, 587, true), context.Background())
	send(newClient(bare.CertPool()), cfgFor(bare, 25, false), context.Background())
	send(newClient(bare.CertPool()), cfgFor(bare, 2525, true), context.Background())
	nodial := cfgFor(bare, 587, true)
	nodial.Dial = nil
	send(newClient(nil), nodial, context.Background())
	hang := testsmtp.New(t, testsmtp.Options{Hang: true})
	c := newClient(hang.CertPool())
	c.Timeout = 100 * time.Millisecond
	send(c, cfgFor(hang, 587, true), context.Background())
	send(c, cfgFor(hang, 465, true), context.Background())
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	send(newClient(hang.CertPool()), cfgFor(hang, 587, true), cctx)
	for _, err := range errs {
		if IsHardBounce(err.Error()) {
			t.Errorf("client-generated error classifies as hard bounce: %q", err)
		}
	}
	if !strings.Contains(errs[0].Error(), "STARTTLS extension not supported by server.") {
		t.Errorf("587 message = %q", errs[0])
	}
}

func TestShortPasswordDoesNotMangleErrors(t *testing.T) {
	err := redact(errors.New("535 5.7.8 authentication failed for abc"), "a")
	if got := err.Error(); got != "535 5.7.8 authentication failed for abc" {
		t.Fatalf("short password mangled text: %q", got)
	}
	if got := redact(errors.New("bad pw1234 here"), "pw1234").Error(); got != "bad [redacted] here" {
		t.Fatalf("got %q", got)
	}
}

func TestEHLOUsesLocalFQDN(t *testing.T) {
	want, _ := os.Hostname()
	if want == "" {
		want = "localhost"
	}
	if got := ehloName(); got != want {
		t.Fatalf("ehloName = %q, want %q", got, want)
	}
}
