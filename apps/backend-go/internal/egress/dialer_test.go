package egress

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
)

func TestNewDialContextRefusesBlockedAndAllowsPublic(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var hits atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			c.Close()
		}
	}()
	addr := ln.Addr().String()
	for _, env := range []string{"production", ""} {
		_, err := NewDialContext(env, []string{"127.0.0.1"})(context.Background(), "tcp", addr)
		if !errors.Is(err, ErrBlocked) {
			t.Fatalf("env %q: err = %v, want ErrBlocked (allowlist ignored outside test/bench)", env, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("blocked dial reached the listener %d times", hits.Load())
	}
	c, err := NewDialContext("test", []string{"127.0.0.1"})(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("allowlisted test host: %v", err)
	}
	c.Close()
}
