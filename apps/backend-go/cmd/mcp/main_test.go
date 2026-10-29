package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
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
	code := run([]string{"--sse", "3100"}, env(map[string]string{"APP_ENV": "test", "OPENGTM_MCP_SSE_HOST": "0.0.0.0"}), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK") || out.Len() != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errb.String())
	}
	// Overridden: passes the bind check; the scaffold then reports no transport.
	errb.Reset()
	code = run([]string{"--sse"}, env(map[string]string{"APP_ENV": "test", "OPENGTM_MCP_SSE_HOST": "0.0.0.0", "OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK": "true"}), &out, &errb)
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

type fakeSecrets struct {
	vals  map[string]string // "ws/key"
	fail  map[string]bool   // key -> lookup/decrypt failure
	calls []string
}

func (f *fakeSecrets) Get(_ context.Context, ws, key string) (string, string, error) {
	f.calls = append(f.calls, ws+"/"+key)
	if f.fail[key] {
		return "", "", errors.New("decrypt failed")
	}
	if v, ok := f.vals[ws+"/"+key]; ok {
		return v, "workspace", nil
	}
	return f.vals["/"+key], "global", nil
}

func TestLiveConfigResolvesPerWorkspaceThroughSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	sec := &fakeSecrets{vals: map[string]string{
		"/REACHER_URL": "https://reacher.internal", "wsA/REACHER_URL": "http://169.254.169.254", // workspace URL must never be asked
		"wsA/REACHER_API_KEY": "key-A", "wsB/REACHER_API_KEY": "key-B", "/REACHER_TIMEOUT": "7",
		"wsA/REACHER_ENABLED": "true", "wsB/REACHER_ENABLED": "false",
	}, fail: map[string]bool{}}
	cfg := liveConfig(nil, env(map[string]string{"EGRESS_ALLOW_HOSTS": "other.internal, " + host}), nil, sec)
	if cfg.HTTP == nil || cfg.ReacherSettings == nil || cfg.Keys == nil {
		t.Fatalf("incomplete: %+v", cfg)
	}
	ctx := context.Background()
	ka, _ := cfg.Keys(ctx, "wsA", "REACHER_API_KEY")
	kb, _ := cfg.Keys(ctx, "wsB", "REACHER_API_KEY")
	if ka != "key-A" || kb != "key-B" {
		t.Fatalf("keys not per workspace: %q %q", ka, kb)
	}
	a, b := cfg.ReacherSettings(ctx, "wsA"), cfg.ReacherSettings(ctx, "wsB")
	if a["REACHER_ENABLED"] != "true" || b["REACHER_ENABLED"] != "false" {
		t.Fatalf("enabled per workspace: %v %v", a, b)
	}
	for _, m := range []map[string]any{a, b} {
		if m["REACHER_URL"] != "https://reacher.internal" || m["REACHER_TIMEOUT"] != "7" {
			t.Fatalf("url/timeout must be global: %v", m)
		}
	}
	for _, c := range sec.calls {
		if c == "wsA/REACHER_URL" || c == "wsB/REACHER_URL" {
			t.Fatalf("REACHER_URL looked up per workspace: %v", sec.calls)
		}
	}
	// Fail closed: a lookup/decrypt failure is an error from Keys (no env
	// fallback) and leaves Reacher disabled for the workspace.
	sec.fail["REACHER_API_KEY"], sec.fail["REACHER_ENABLED"] = true, true
	if k, err := cfg.Keys(ctx, "wsA", "REACHER_API_KEY"); err == nil || k != "" {
		t.Fatalf("decrypt failure must error: %q %v", k, err)
	}
	if m := cfg.ReacherSettings(ctx, "wsA"); m["REACHER_ENABLED"] != nil {
		t.Fatalf("failed enabled lookup must not enable Reacher: %v", m)
	}
	// The operator allow-list opens exactly the listed private target.
	resp, err := cfg.HTTP.Get(srv.URL)
	if err != nil {
		t.Fatalf("allow-listed host refused: %v", err)
	}
	resp.Body.Close()
	if r, err := liveConfig(nil, env(nil), nil, sec).HTTP.Get(srv.URL); err == nil {
		r.Body.Close()
		t.Fatal("loopback target reachable without EGRESS_ALLOW_HOSTS")
	}
}

func TestMainRefusesNonLoopbackWithoutAuth(t *testing.T) {
	var out, errb bytes.Buffer
	called := false
	old := startSSE
	startSSE = func(context.Context, config.MCP, func(string) string, string, int, io.Writer) error {
		called = true
		return nil
	}
	t.Cleanup(func() { startSSE = old })
	code := run([]string{"--sse", "3100"}, env(map[string]string{
		"APP_ENV": "test", "OPENGTM_MCP_SSE_HOST": "0.0.0.0", "OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK": "true", "MCP_REQUIRE_AUTH": "false",
	}), &out, &errb)
	if code != 1 || called || !strings.Contains(errb.String(), "MCP_REQUIRE_AUTH") || out.Len() != 0 {
		t.Fatalf("code=%d called=%v err=%q", code, called, errb.String())
	}
	// keyless on loopback stays allowed
	errb.Reset()
	code = run([]string{"--sse", "3100"}, env(map[string]string{"APP_ENV": "test", "MCP_REQUIRE_AUTH": "false"}), &out, &errb)
	if code != 0 || !called {
		t.Fatalf("loopback keyless: code=%d called=%v err=%q", code, called, errb.String())
	}
}

// R-secret: the MCP server refuses to start without a strong SECRET_KEY unless APP_ENV=test.
func TestMainRefusesWeakSecretKey(t *testing.T) {
	for _, m := range []map[string]string{
		{}, {"APP_ENV": "dev"}, {"APP_ENV": "production", "SECRET_KEY": "short"}, {"APP_ENV": "local", "SECRET_KEY": "short"},
	} {
		m["OPENGTM_MCP_TOKEN"] = "t"
		var out, errb bytes.Buffer
		if code := run([]string{"--sse", "3100"}, env(m), &out, &errb); code != 1 || !strings.Contains(errb.String(), "SECRET_KEY") {
			t.Fatalf("env %v: code=%d stderr=%q", m, code, errb.String())
		}
	}
}
