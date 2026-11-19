package emailverify

import (
	"context"
	"net"
	"testing"
)

type okSession struct{ rcpt int }

func (okSession) Connect(context.Context, string, int) error { return nil }
func (okSession) EhloOrHelo(string) error                    { return nil }
func (okSession) Helo(string) error                          { return nil }
func (okSession) Mail(string) (int, error)                   { return 250, nil }
func (s okSession) Rcpt(a string) (int, error) {
	if a == "ghost@acme.test" {
		return 550, nil
	}
	if a[:5] == "abcde" { // the random catch-all calibration address
		return 550, nil
	}
	return s.rcpt, nil
}
func (okSession) Quit() error { return nil }
func (okSession) Close()      {}

type oneMX struct{}

func (oneMX) LookupMX(context.Context, string) ([]*net.MX, error) {
	return []*net.MX{{Pref: 10, Host: "mx.acme.test."}}, nil
}

func probeDeps(sess SMTPSession) Deps {
	d := NewDeps()
	d.Resolver = oneMX{}
	// Wired after NewDeps, the way NewLive does it.
	d.NewSession = func() SMTPSession { return sess }
	d.RandLocal = func() string { return "abcdefghijklmnop" }
	return d
}

// The registry's SMTP verifier used to be bound to the Deps as NewDeps made them, before NewLive set NewSession, so
// every cascade probe ended as "unreachable" without a single connection. The verifier must pick up the live Deps.
func TestCascadeSMTPVerifierUsesTheDepsItIsRunWith(t *testing.T) {
	d := probeDeps(okSession{rcpt: 250})
	got, err := Cascade(context.Background(), d, "ws", "real@acme.test")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusValid || got.Detail != "rcpt_accepted" {
		t.Fatalf("got %s/%s, want valid/rcpt_accepted (the probe never ran)", got.Status, got.Detail)
	}
	got, _ = Cascade(context.Background(), d, "ws", "ghost@acme.test")
	if got.Status != StatusInvalid {
		t.Fatalf("got %s/%s, want invalid", got.Status, got.Detail)
	}
}

func TestCascadeHonoursSMTPDisabledSetAfterNewDeps(t *testing.T) {
	d := probeDeps(okSession{rcpt: 250})
	d.SMTPDisabled = true
	got, err := Cascade(context.Background(), d, "ws", "real@acme.test")
	if err != nil {
		t.Fatal(err)
	}
	if got.Detail != "no_verifier" {
		t.Fatalf("got %s/%s: with probing off there is no verifier in the chain", got.Status, got.Detail)
	}
}
