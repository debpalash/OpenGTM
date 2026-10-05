package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestDialGuardedAppliesTheDestinationGuard(t *testing.T) {
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
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	addr := ln.Addr().String()

	strict, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, target := range []string{addr, "localhost:443", "169.254.169.254:80", "[::1]:443", "2130706433:443", "0x7f.1:443", "10.1.2.3:22"} {
		conn, err := strict.DialGuarded(ctx, "tcp", target)
		if err == nil {
			conn.Close()
			t.Errorf("%s: dial succeeded through the guard", target)
			continue
		}
		var be *BlockedError
		if !errors.As(err, &be) {
			t.Errorf("%s: want a BlockedError, got %v", target, err)
		}
	}
	if _, err := strict.DialGuarded(ctx, "udp", "example.com:53"); err == nil {
		t.Error("non-TCP dial accepted")
	}

	// With the test escape hatch the same dial works and carries bytes.
	open, err := New(Options{AllowPrivateForTesting: true})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := open.DialGuarded(ctx, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo: %q %v", buf, err)
	}
}
