package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestParseTransport(t *testing.T) {
	for _, tc := range []struct {
		args []string
		sse  bool
		port int
		ok   bool
	}{
		{nil, false, 0, true},
		{[]string{"--sse"}, true, 3100, true},
		{[]string{"--sse", "4000"}, true, 4000, true},
		{[]string{"--sse", "abc"}, true, 3100, true}, // Python: non-digit means default port
		{[]string{"--sse", "70000"}, false, 0, false},
		{[]string{"--sse", "0"}, false, 0, false},
	} {
		sse, port, err := parseTransport(tc.args)
		if (err == nil) != tc.ok || (tc.ok && (sse != tc.sse || port != tc.port)) {
			t.Errorf("%v: sse=%v port=%d err=%v", tc.args, sse, port, err)
		}
	}
}

func TestMainRefusesNonLoopbackWithoutOverride(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"--sse", "3100"}, env(map[string]string{"OPENGTM_MCP_SSE_HOST": "0.0.0.0"}), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK") || out.Len() != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errb.String())
	}
	// Overridden: passes the bind check; the scaffold then reports no transport.
	errb.Reset()
	code = run([]string{"--sse"}, env(map[string]string{"OPENGTM_MCP_SSE_HOST": "0.0.0.0", "OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK": "true"}), &out, &errb)
	if code != 1 || strings.Contains(errb.String(), "NONLOOPBACK") || out.Len() != 0 {
		t.Fatalf("override: code=%d err=%q", code, errb.String())
	}
}

func TestMainNoTransportExitsOne(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, env(nil), &out, &errb); code != 1 || errb.Len() == 0 || out.Len() != 0 {
		t.Fatalf("code=%d err=%q out=%q", code, errb.String(), out.String())
	}
}

func TestLoopback(t *testing.T) {
	for h, want := range map[string]bool{"127.0.0.1": true, "::1": true, "localhost": true, "127.1.2.3": true, "0.0.0.0": false, "10.0.0.1": false, "example.com": false, "": false} {
		if loopbackHost(h) != want {
			t.Errorf("%q", h)
		}
	}
}

func TestLiveConfigWiresOperatorReacherAndEgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	get := env(map[string]string{
		"REACHER_URL": "https://reacher.internal", "REACHER_ENABLED": "true", "REACHER_TIMEOUT": "7",
		"REACHER_API_KEY": "operator-key", "EGRESS_ALLOW_HOSTS": "other.internal, " + host,
	})
	cfg := liveConfig(nil, get, nil)
	if cfg.HTTP == nil || cfg.ReacherSettings == nil || cfg.Keys == nil {
		t.Fatalf("incomplete: %+v", cfg)
	}
	// Settings are global: the same for every workspace, never read per workspace.
	for _, ws := range []string{"a", "b"} {
		s := cfg.ReacherSettings(context.Background(), ws)
		if s["REACHER_URL"] != "https://reacher.internal" || s["REACHER_ENABLED"] != "true" || s["REACHER_TIMEOUT"] != "7" {
			t.Fatalf("%s: %v", ws, s)
		}
	}
	if k, _ := cfg.Keys(context.Background(), "ws", "REACHER_API_KEY"); k != "operator-key" {
		t.Fatalf("key %q", k)
	}
	// The operator allow-list opens exactly the listed private target.
	resp, err := cfg.HTTP.Get(srv.URL)
	if err != nil {
		t.Fatalf("allow-listed host refused: %v", err)
	}
	resp.Body.Close()
	unlisted := liveConfig(nil, env(nil), nil)
	if r, err := unlisted.HTTP.Get(srv.URL); err == nil {
		r.Body.Close()
		t.Fatal("loopback target reachable without EGRESS_ALLOW_HOSTS")
	}
}
