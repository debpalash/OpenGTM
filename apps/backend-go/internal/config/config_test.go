package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

const goodTok = "0123456789abcdef0123456789abcdef"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	c, err := load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": goodTok}))
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkerConcurrency != 4 || c.DBMaxConns != 10 || c.ShutdownGrace != 30*time.Second || c.HeartbeatInterval != 30*time.Second {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestRejects(t *testing.T) {
	cases := map[string]map[string]string{
		"empty token":        {},
		"concurrency >64":    {"INTERNAL_SERVICE_TOKEN": goodTok, "GO_WORKER_CONCURRENCY": "65"},
		"grace >300":         {"INTERNAL_SERVICE_TOKEN": goodTok, "GO_WORKER_SHUTDOWN_GRACE_SECONDS": "301"},
		"egress in prod":     {"INTERNAL_SERVICE_TOKEN": goodTok, "APP_ENV": "production", "EGRESS_ALLOW_HOSTS": "a.test"},
		"egress without env": {"INTERNAL_SERVICE_TOKEN": goodTok, "EGRESS_ALLOW_HOSTS": "a.test"},
	}
	for name, m := range cases {
		if _, err := load(env(m)); err != nil {
			continue
		}
		t.Errorf("%s: expected error", name)
	}
}

func TestEgressAllowedInBench(t *testing.T) {
	c, err := load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": goodTok, "APP_ENV": "bench", "EGRESS_ALLOW_HOSTS": "a.test, b.test"}))
	if err != nil || len(c.EgressAllowHosts) != 2 {
		t.Fatalf("got %+v, %v", c, err)
	}
}

func TestOverridesAndDebugDefault(t *testing.T) {
	c, err := load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": goodTok, "GO_WORKER_DB_MAX_CONNS": "7", "GO_WORKER_HEARTBEAT_SECONDS": "5"}))
	if err != nil || c.DBMaxConns != 7 || c.HeartbeatInterval != 5*time.Second || c.DebugAddr != "127.0.0.1:6060" {
		t.Fatalf("got %+v, %v", c, err)
	}
}

func TestSecretsRedacted(t *testing.T) {
	c, err := load(env(map[string]string{
		"INTERNAL_SERVICE_TOKEN": "tok-SECRET-123-0123456789abcdefghijklmnop",
		"DATABASE_URL":           "postgres://u:dbPASS-9@h:5432/d",
		"REDIS_URL":              "redis://:redisPASS-7@h:6379/0",
	}))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cfg", "config", c)
	slog.New(slog.NewTextHandler(&buf, nil)).Info("cfg", "config", c)
	outs := []string{fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%v", &c), fmt.Sprintf("%s", c), buf.String()}
	for _, o := range outs {
		for _, secret := range []string{"tok-SECRET-123", "dbPASS-9", "redisPASS-7"} {
			if strings.Contains(o, secret) {
				t.Errorf("leaked %q in %q", secret, o)
			}
		}
	}
	if !strings.Contains(outs[0], "h:5432/d") {
		t.Errorf("redaction should keep host: %s", outs[0])
	}
}

func TestMaxActivePerWorkspace(t *testing.T) {
	get := func(v string) (int, error) {
		m := map[string]string{"INTERNAL_SERVICE_TOKEN": goodTok}
		if v != "" {
			m["WORKER_MAX_ACTIVE_PER_WORKSPACE"] = v
		}
		c, err := load(env(m))
		return c.MaxActivePerWorkspace, err
	}
	for in, want := range map[string]int{"": 2, "0": 0, "5": 5, "64": 64, "100": 64, "-3": 0} {
		if got, err := get(in); err != nil || got != want {
			t.Errorf("%q: got %d err %v want %d", in, got, err, want)
		}
	}
	if _, err := get("abc"); err == nil {
		t.Error("expected error for non-integer")
	}
}

func TestWeakServiceToken(t *testing.T) {
	for name, tok := range map[string]string{
		"short":       "short-token",
		"31 chars":    strings.Repeat("a", 31),
		"change-me":   "change-me-" + strings.Repeat("x", 40),
		"change-me-2": "change-me",
	} {
		if _, err := load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": tok})); err == nil {
			t.Errorf("%s: weak token accepted", name)
		}
	}
	if _, err := load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": strings.Repeat("a", 32)})); err != nil {
		t.Errorf("32-char token rejected: %v", err)
	}
}

func TestJobTimeoutDefaultAndClamp(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"": 1800 * time.Second, "30": 60 * time.Second, "99999": 7200 * time.Second, "900": 900 * time.Second,
	} {
		c, err := load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": goodTok, "GO_WORKER_JOB_TIMEOUT_SECONDS": in}))
		if err != nil || c.JobTimeout != want {
			t.Errorf("%q: got %v, %v want %v", in, c.JobTimeout, err, want)
		}
	}
	if _, err := load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": goodTok, "GO_WORKER_JOB_TIMEOUT_SECONDS": "x"})); err == nil {
		t.Error("non-integer accepted")
	}
}

func TestRedactMasksPasswordQueryParam(t *testing.T) {
	got := redactURL("postgres://u@h:5432/d?sslmode=disable&password=hunter2")
	if strings.Contains(got, "hunter2") || !strings.Contains(got, "sslmode=disable") {
		t.Fatalf("got %q", got)
	}
}
