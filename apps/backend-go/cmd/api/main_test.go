package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
)

func TestMainBuildsRouter(t *testing.T) {
	cfg := config.APIConfig{SecretKey: "k", CORSOrigins: "*", AppEnv: "test"}
	h := buildRouter(cfg, telemetry.NewLogger(io.Discard), nil).Handler()

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
