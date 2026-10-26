package egress

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

// The SMTP/IMAP dial guard is NewDialContext (dialer.go); these tests pin the
// behaviour plan 09 relies on for customer-configured mail hosts.

func TestDialRejectsPrivateLoopbackLinkLocalMapped(t *testing.T) {
	blockedHosts := []string{
		"10.0.0.5", "172.16.0.1", "192.168.1.1", "127.0.0.1", "169.254.169.254",
		"100.64.0.1", "0.0.0.0", "::1", "fe80::1", "fc00::1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.5", "64:ff9b::7f00:1", "2130706433", "0x7f000001",
		"localhost",
	}
	for _, h := range blockedHosts {
		called := false
		dial := guardedDial(map[string]struct{}{}, &fakeResolver{seq: [][]netip.Addr{addrs("127.0.0.1")}},
			func(context.Context, string, string) (net.Conn, error) { called = true; return nil, errors.New("must not dial") })
		_, err := dial(context.Background(), "tcp", net.JoinHostPort(h, "587"))
		if !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: err = %v, want ErrBlocked", h, err)
		}
		if called {
			t.Errorf("%s: dial reached", h)
		}
	}
	// A hostname resolving to a blocked address is refused as well.
	for _, ip := range []string{"10.1.2.3", "100.100.0.1", "::ffff:192.168.0.1", "169.254.1.1"} {
		dial := guardedDial(map[string]struct{}{}, &fakeResolver{seq: [][]netip.Addr{addrs("93.184.216.34", ip)}},
			func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("must not dial") })
		if _, err := dial(context.Background(), "tcp", "mail.example:587"); !errors.Is(err, ErrBlocked) {
			t.Errorf("resolves to %s: err = %v, want ErrBlocked", ip, err)
		}
	}
}

func TestDialRebindBlocked(t *testing.T) {
	res := &fakeResolver{seq: [][]netip.Addr{addrs("93.184.216.34"), addrs("127.0.0.1")}}
	var dialed []string
	dial := guardedDial(map[string]struct{}{}, res, func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return nil, errors.New("fake dial")
	})
	_, _ = dial(context.Background(), "tcp", "rebind.example:465")
	if len(dialed) != 1 || dialed[0] != "93.184.216.34:465" {
		t.Fatalf("dialed %v, want the vetted IP literal", dialed)
	}
	if _, err := dial(context.Background(), "tcp", "rebind.example:465"); !errors.Is(err, ErrBlocked) || len(dialed) != 1 {
		t.Fatalf("rebind: err=%v dialed=%v", err, dialed)
	}
}

func TestDialAllowlistIgnoredOutsideBench(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	addr := ln.Addr().String()
	for _, env := range []string{"", "production", "development", "staging"} {
		if _, err := NewDialContext(env, []string{"127.0.0.1"})(context.Background(), "tcp", addr); !errors.Is(err, ErrBlocked) {
			t.Errorf("env %q: err = %v, want ErrBlocked", env, err)
		}
	}
	for _, env := range []string{"test", "bench"} {
		c, err := NewDialContext(env, []string{"127.0.0.1"})(context.Background(), "tcp", addr)
		if err != nil {
			t.Errorf("env %q: %v", env, err)
			continue
		}
		c.Close()
	}
}
