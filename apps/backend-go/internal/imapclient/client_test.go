package imapclient

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachcfg"
	"github.com/emersion/go-imap/v2"
)

const msgA = "From: mailer-daemon@x.example\r\nTo: bounces@sender.example\r\nSubject: bounce A\r\n\r\nbody A\r\n"

func toAddr(addr string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
}

func cfg(useSSL bool) outreachcfg.IMAP {
	return outreachcfg.IMAP{Host: "localhost", Port: 993, User: testUser, Password: config.Secret(testPassword), Folder: "INBOX", UseSSL: useSSL}
}

func connected(t *testing.T, ts *testServer) Client {
	t.Helper()
	c := NewWithOptions(cfg(true), toAddr(ts.addr), 5*time.Second, Options{RootCAs: ts.pool})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Logout)
	return c
}

var ctx = context.Background()

func TestSelectReturnsUIDValidity(t *testing.T) {
	ts := newServer(t, modeImplicitTLS)
	c := connected(t, ts)
	v, err := c.Select(ctx, "INBOX")
	if err != nil || v != "1" {
		t.Fatalf("uidvalidity=%q err=%v", v, err)
	}
	if _, err := c.Select(ctx, "Nope"); err == nil {
		t.Fatal("selecting a missing folder must fail")
	}
}

func TestSearchUnseenSince(t *testing.T) {
	ts := newServer(t, modeImplicitTLS)
	now := time.Now()
	ts.add(t, msgA, now.Add(-72*time.Hour))              // uid 1: old, unseen
	ts.add(t, msgA, now.Add(-1*time.Hour))               // uid 2: recent, unseen
	ts.add(t, msgA, now.Add(-1*time.Hour), imap.FlagSeen) // uid 3: recent, seen
	c := connected(t, ts)
	if _, err := c.Select(ctx, "INBOX"); err != nil {
		t.Fatal(err)
	}
	got, err := c.SearchUnseen(ctx, now.Add(-24*time.Hour))
	if err != nil || len(got) != 1 || got[0] != "2" {
		t.Fatalf("got %v err %v", got, err)
	}
	got, err = c.SearchUnseen(ctx, now.Add(-240*time.Hour))
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v err %v", got, err)
	}
	empty, err := c.SearchUnseen(ctx, now.Add(24*time.Hour))
	if err != nil || len(empty) != 0 {
		t.Fatalf("future since: %v %v", empty, err)
	}
}

func TestFetchReturnsRFC822(t *testing.T) {
	ts := newServer(t, modeImplicitTLS)
	ts.add(t, msgA, time.Now())
	c := connected(t, ts)
	c.Select(ctx, "INBOX")
	b, err := c.Fetch(ctx, "1")
	if err != nil || string(b) != msgA {
		t.Fatalf("fetch = %q err %v", b, err)
	}
	if b, err := c.Fetch(ctx, "99"); err != nil || len(b) != 0 {
		t.Fatalf("missing uid should be empty, got %q %v", b, err)
	}
	if _, err := c.Fetch(ctx, "abc"); err == nil {
		t.Fatal("non-numeric uid must error")
	}
}

func TestFetchOversizedIsEmpty(t *testing.T) {
	ts := newServer(t, modeImplicitTLS)
	big := "Subject: big\r\n\r\n" + strings.Repeat("x", MaxFetchBytes) + "\r\n"
	ts.add(t, big, time.Now())
	ok := "Subject: ok\r\n\r\n" + strings.Repeat("y", MaxFetchBytes-100)
	ts.add(t, ok, time.Now())
	c := connected(t, ts)
	c.Select(ctx, "INBOX")
	if b, err := c.Fetch(ctx, "1"); err != nil || len(b) != 0 {
		t.Fatalf("oversized: len=%d err=%v", len(b), err)
	}
	if b, err := c.Fetch(ctx, "2"); err != nil || string(b) != ok {
		t.Fatalf("at-cap message lost: len=%d err=%v", len(b), err)
	}
}

func TestFetchDoesNotSetSeenButMarkSeenDoes(t *testing.T) {
	ts := newServer(t, modeImplicitTLS)
	ts.add(t, msgA, time.Now())
	c := connected(t, ts)
	c.Select(ctx, "INBOX")
	if _, err := c.Fetch(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	if u, _ := c.SearchUnseen(ctx, time.Now().Add(-time.Hour)); len(u) != 1 {
		t.Fatalf("fetch must not mark seen: %v", u)
	}
	if err := c.MarkSeen(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	if u, _ := c.SearchUnseen(ctx, time.Now().Add(-time.Hour)); len(u) != 0 {
		t.Fatalf("still unseen after MarkSeen: %v", u)
	}
	if b, _ := c.Fetch(ctx, "1"); string(b) != msgA {
		t.Fatal("MarkSeen must not alter the message")
	}
}

func TestMarkSeenSetsFlagOnly(t *testing.T) {
	ts := newServer(t, modeImplicitTLS)
	ts.add(t, msgA, time.Now(), imap.FlagFlagged)
	c := connected(t, ts)
	c.Select(ctx, "INBOX")
	if err := c.MarkSeen(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	// +FLAGS keeps \Flagged; assert through a raw second client.
	c2 := connected(t, ts)
	c2.Select(ctx, "INBOX")
	impl := c2.(*client)
	msgs, err := impl.c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{Flags: true, UID: true}).Collect()
	if err != nil || len(msgs) != 1 {
		t.Fatalf("%v %v", msgs, err)
	}
	var seen, flagged bool
	for _, f := range msgs[0].Flags {
		seen = seen || f == imap.FlagSeen
		flagged = flagged || f == imap.FlagFlagged
	}
	if !seen || !flagged {
		t.Fatalf("flags = %v", msgs[0].Flags)
	}
}

func TestLoginFailureError(t *testing.T) {
	ts := newServer(t, modeImplicitTLS)
	bad := cfg(true)
	bad.Password = config.Secret("wrong-password-xyz")
	c := NewWithOptions(bad, toAddr(ts.addr), 5*time.Second, Options{RootCAs: ts.pool})
	err := c.Connect(ctx)
	if err == nil {
		t.Fatal("expected login failure")
	}
	if strings.Contains(err.Error(), "wrong-password-xyz") {
		t.Fatalf("password leaked: %v", err)
	}
	if _, err := c.Select(ctx, "INBOX"); err == nil {
		t.Fatal("calls after failed connect must error, not panic")
	}
	c.Logout()
}

func TestSTARTTLSWhenNotImplicit(t *testing.T) {
	ts := newServer(t, modeSTARTTLS)
	c := NewWithOptions(cfg(false), toAddr(ts.addr), 5*time.Second, Options{RootCAs: ts.pool})
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Logout()
	if v, err := c.Select(ctx, "INBOX"); err != nil || v != "1" {
		t.Fatalf("%q %v", v, err)
	}
}

func TestPlaintextRefusedUnlessAllowListed(t *testing.T) {
	ts := newServer(t, modePlain)
	// No STARTTLS on the server: not UseSSL and not allow-listed -> refused, no LOGIN sent.
	c := NewWithOptions(cfg(false), toAddr(ts.addr), 5*time.Second, Options{RootCAs: ts.pool})
	if err := c.Connect(ctx); err == nil {
		t.Fatal("plaintext connection must be refused")
	}
	c.Logout()
	ok := NewWithOptions(cfg(false), toAddr(ts.addr), 5*time.Second, Options{PlainHosts: []string{"localhost"}})
	if err := ok.Connect(ctx); err != nil {
		t.Fatalf("allow-listed plain host: %v", err)
	}
	ok.Logout()
}

func TestCertificateVerificationRequired(t *testing.T) {
	ts := newServer(t, modeImplicitTLS)
	c := NewWithOptions(cfg(true), toAddr(ts.addr), 5*time.Second, Options{}) // no RootCAs: self-signed must fail
	err := c.Connect(ctx)
	if err == nil {
		t.Fatal("self-signed certificate was accepted")
	}
	if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "x509") {
		t.Fatalf("unexpected error: %v", err)
	}
	// STARTTLS path verifies too.
	ts2 := newServer(t, modeSTARTTLS)
	c2 := NewWithOptions(cfg(false), toAddr(ts2.addr), 5*time.Second, Options{})
	if err := c2.Connect(ctx); err == nil {
		t.Fatal("self-signed certificate accepted over STARTTLS")
	}
}

func TestDialGuardUsed(t *testing.T) {
	ts := newServer(t, modeImplicitTLS)
	host, port, _ := net.SplitHostPort(ts.addr)
	c := cfg(true)
	c.Host = host
	c.Port = atoi(port)
	guard := egress.NewDialContext("production", nil)
	cl := NewWithOptions(c, guard, 5*time.Second, Options{RootCAs: ts.pool})
	if err := cl.Connect(ctx); err == nil {
		t.Fatal("loopback must be refused by the egress guard")
	}
	if n := ts.accepts.Load(); n != 0 {
		t.Fatalf("blocked IP still connected (%d accepts)", n)
	}
}

func TestNilDialerRefused(t *testing.T) {
	c := New(cfg(true), nil, time.Second)
	if err := c.Connect(ctx); err == nil || !strings.Contains(err.Error(), "dial") {
		t.Fatalf("nil dialer must be refused, got %v", err)
	}
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

func TestContextCancelAborts(t *testing.T) {
	ts := newServer(t, modeImplicitTLS)
	hang, pool := rawServer(t, func(tag, cmd string) string { return "" }) // never answers commands
	_ = ts
	c := NewWithOptions(cfg(true), toAddr(hang), time.Minute, Options{RootCAs: pool})
	cctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	err := c.Connect(cctx)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("cancel did not abort promptly")
	}
}

func TestTimeoutBoundsCalls(t *testing.T) {
	hang, pool := rawServer(t, func(tag, cmd string) string { return "" })
	c := NewWithOptions(cfg(true), toAddr(hang), 300*time.Millisecond, Options{RootCAs: pool})
	start := time.Now()
	if err := c.Connect(ctx); err == nil {
		t.Fatal("expected timeout")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout not enforced")
	}
}

func TestErrorNeverContainsPassword(t *testing.T) {
	// A hostile server echoes the password (and its base64) in the LOGIN rejection.
	addr, pool := rawServer(t, func(tag, cmd string) string {
		return tag + " NO login failed for " + testPassword + " cHc6 " + "aW1hcC1zM2NyZXQtRE8tTk9ULUxFQUs=" + "\r\n"
	})
	c := NewWithOptions(cfg(true), toAddr(addr), 5*time.Second, Options{RootCAs: pool})
	err := c.Connect(ctx)
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, leak := range []string{testPassword, "aW1hcC1zM2NyZXQtRE8tTk9ULUxFQUs="} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("leaked %q in %q", leak, err)
		}
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("expected redaction marker: %v", err)
	}
}

func TestProductionConstructorHasNoTestHatches(t *testing.T) {
	c, ok := New(cfg(true), toAddr("x"), time.Second).(*client)
	if !ok {
		t.Fatal("New must return *client")
	}
	if c.opts.RootCAs != nil || len(c.opts.PlainHosts) != 0 {
		t.Fatalf("production constructor must leave test options empty: %+v", c.opts)
	}
}
