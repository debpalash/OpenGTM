package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/api"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
)

func mustRouter(t *testing.T, cfg config.APIConfig) http.Handler {
	t.Helper()
	r, err := buildRouter(cfg, telemetry.NewLogger(io.Discard), nil, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return r.Handler()
}

func TestSettingsDepsFailStartupOnBadConfig(t *testing.T) {
	cfg := config.APIConfig{SecretKey: "k", AppEnv: "test"}
	t.Setenv("SECRETS_PROVIDER", "bogus-provider")
	t.Setenv("SECRETS_MASTER_KEY", "master-key-must-not-leak")
	_, err := newSettingsDeps(cfg, settings.Load)
	if err == nil || !strings.Contains(err.Error(), "SECRETS_PROVIDER") {
		t.Fatalf("bad provider must fail startup, got %v", err)
	}
	if strings.Contains(err.Error(), "master-key-must-not-leak") {
		t.Fatal("error leaks a secret")
	}

	t.Setenv("SECRETS_PROVIDER", "local")
	if _, err := newSettingsDeps(cfg, func() (*settings.Registry, error) { return nil, errors.New("boom") }); err == nil || !strings.Contains(err.Error(), "registry") {
		t.Fatalf("registry load failure must fail startup, got %v", err)
	}
	d, err := newSettingsDeps(cfg, settings.Load)
	if err != nil || d.Registry == nil || d.Envelope == nil || d.Egress == nil {
		t.Fatalf("valid config: %+v %v", d, err)
	}
}

func TestMainBuildsRouter(t *testing.T) {
	cfg := config.APIConfig{SecretKey: "k", CORSOrigins: "*", AppEnv: "test"}
	h := mustRouter(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("healthz: %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Error("RequestID middleware not applied")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/nope", nil))
	if rec.Code != http.StatusNotFound || strings.TrimSpace(rec.Body.String()) != `{"detail":"Not Found"}` {
		t.Fatalf("404: %d %q", rec.Code, rec.Body.String())
	}
}

func TestHealthURLFromAddr(t *testing.T) {
	for in, want := range map[string]string{
		"":             "http://127.0.0.1:8000/healthz",
		":8000":        "http://127.0.0.1:8000/healthz",
		":9123":        "http://127.0.0.1:9123/healthz",
		"0.0.0.0:7000": "http://127.0.0.1:7000/healthz",
		"[::]:7001":    "http://127.0.0.1:7001/healthz",
		"junk":         "http://127.0.0.1:8000/healthz",
	} {
		if got := healthURL(in); got != want {
			t.Errorf("healthURL(%q) = %q want %q", in, got, want)
		}
	}
}

func TestHealthcheckExitCodes(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"status":"ok"}`)) }))
	defer ok.Close()
	if err := healthcheck(ok.URL); err != nil {
		t.Errorf("healthy: %v", err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer bad.Close()
	if err := healthcheck(bad.URL); err == nil {
		t.Error("503 must fail")
	}
	if err := healthcheck("http://127.0.0.1:1/healthz"); err == nil {
		t.Error("connection refused must fail")
	}
}

func TestServerTimeouts(t *testing.T) {
	s := newServer(":0", http.NotFoundHandler())
	if s.ReadTimeout != 30*time.Second || s.IdleTimeout != 120*time.Second || s.WriteTimeout != 0 || s.ReadHeaderTimeout != 10*time.Second {
		t.Fatalf("timeouts: %v %v %v %v", s.ReadTimeout, s.IdleTimeout, s.WriteTimeout, s.ReadHeaderTimeout)
	}
}

func TestApplySSEWiresConfigRedisAndShutdown(t *testing.T) {
	ctx := context.Background()
	cfg := config.APIConfig{SSEMaxStreams: 7, SSEMaxPerUser: 2, SSEMaxPerWorkspace: 3, SSEMaxLifetime: time.Minute, WSMaxMessageBytes: 4096,
		RedisURL: "redis://:pw-secret@127.0.0.1:1/0"}
	var d api.Deps
	if err := applySSE(&d, cfg, ctx); err != nil {
		t.Fatal(err)
	}
	if d.Redis == nil || d.SSEMaxStreams != 7 || d.SSEMaxPerUser != 2 || d.SSEMaxPerWorkspace != 3 || d.SSEMaxLifetime != time.Minute || d.ShutdownCtx != ctx || d.WSMaxMessageBytes != 4096 {
		t.Fatalf("deps = %+v", d)
	}
	_ = d.Redis.Close()
	var empty api.Deps
	if err := applySSE(&empty, config.APIConfig{}, ctx); err != nil || empty.Redis != nil {
		t.Fatalf("no REDIS_URL must leave Redis nil: %v", err)
	}
	cfg.RedisURL = "://pw-secret"
	err := applySSE(&api.Deps{}, cfg, ctx)
	if err == nil || strings.Contains(err.Error(), "pw-secret") {
		t.Fatalf("bad REDIS_URL must fail without leaking: %v", err)
	}
}
