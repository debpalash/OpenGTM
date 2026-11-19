package emailverify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// A resolver that gives every domain its own mail server.
type perDomainMX struct{}

func (perDomainMX) LookupMX(_ context.Context, d string) ([]*net.MX, error) {
	return []*net.MX{{Pref: 10, Host: "mx." + d + "."}}, nil
}

type deadSession struct{ reach map[string]bool }

func (s deadSession) Connect(_ context.Context, host string, _ int) error {
	if s.reach[host] {
		return nil
	}
	return errors.New("dial tcp: i/o timeout")
}
func (deadSession) EhloOrHelo(string) error  { return nil }
func (deadSession) Helo(string) error        { return nil }
func (deadSession) Mail(string) (int, error) { return 250, nil }
func (deadSession) Rcpt(a string) (int, error) {
	if strings.HasPrefix(a, "abcde") {
		return 550, nil
	}
	return 250, nil
}
func (deadSession) Quit() error { return nil }
func (deadSession) Close()      {}

func portDeps(reach map[string]bool) Deps {
	d := NewDeps()
	d.Resolver = perDomainMX{}
	d.NewSession = func() SMTPSession { return deadSession{reach} }
	d.RandLocal = func() string { return "abcdefghijklmnop" }
	return d
}

func detail(t *testing.T, d Deps, email string) Result {
	t.Helper()
	r, err := Cascade(context.Background(), d, "ws", email)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// One silent mail server is just that server; several different ones with none answering is a blocked port 25.
func TestPort25BlockedNeedsSeveralDistinctServersAndNoAnswer(t *testing.T) {
	d := portDeps(nil)
	for i := 0; i < port25MinHosts-1; i++ {
		r := detail(t, d, fmt.Sprintf("a@dom%d.test", i))
		if r.Status != StatusUnknown || r.Detail != "unreachable" {
			t.Fatalf("server %d: %s/%s, want unknown/unreachable (not enough evidence yet)", i, r.Status, r.Detail)
		}
	}
	// The same server again does not add evidence.
	if r := detail(t, d, "b@dom0.test"); r.Detail != "unreachable" {
		t.Fatalf("repeat of one server: %s", r.Detail)
	}
	r := detail(t, d, fmt.Sprintf("a@dom%d.test", port25MinHosts))
	if r.Status != StatusUnknown || r.Detail != DetailPort25Blocked {
		t.Fatalf("%s/%s, want unknown/%s", r.Status, r.Detail, DetailPort25Blocked)
	}
	if !d.Port25Blocked() {
		t.Fatal("Port25Blocked false")
	}
}

// Once any mail server answers, port 25 is reachable: later failures are those servers' own, not a blocked port.
func TestPort25NotBlockedWhenAServerAnswers(t *testing.T) {
	d := portDeps(map[string]bool{"mx.up.test": true})
	if r := detail(t, d, "a@up.test"); r.Status == StatusUnknown && r.Detail == DetailPort25Blocked {
		t.Fatalf("a reachable server reported as blocked: %+v", r)
	}
	for i := 0; i < port25MinHosts+2; i++ {
		detail(t, d, fmt.Sprintf("a@dead%d.test", i))
	}
	if d.Port25Blocked() {
		t.Fatal("blocked although a server answered inside the window")
	}
}

func TestPort25EvidenceExpires(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	var c connTrack
	for i := 0; i < port25MinHosts; i++ {
		c.note(fmt.Sprintf("mx%d", i), false, now)
	}
	if !c.blocked(now) {
		t.Fatal("not blocked with enough failures")
	}
	if c.blocked(now.Add(port25Window + time.Second)) {
		t.Fatal("old failures must not keep reporting a blocked port")
	}
}

// A detail that says the port is blocked is not cached as if it were a verdict about the address.
func TestPort25DetailIsTransient(t *testing.T) {
	if !(Result{Status: StatusUnknown, Detail: DetailPort25Blocked}).Transient() {
		t.Fatal("smtp_port_blocked must be transient")
	}
}
