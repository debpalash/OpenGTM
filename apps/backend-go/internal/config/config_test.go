package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	c, err := load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": "x"}))
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
		"concurrency >64":    {"INTERNAL_SERVICE_TOKEN": "x", "GO_WORKER_CONCURRENCY": "65"},
		"grace >300":         {"INTERNAL_SERVICE_TOKEN": "x", "GO_WORKER_SHUTDOWN_GRACE_SECONDS": "301"},
		"egress in prod":     {"INTERNAL_SERVICE_TOKEN": "x", "APP_ENV": "production", "EGRESS_ALLOW_HOSTS": "a.test"},
		"egress without env": {"INTERNAL_SERVICE_TOKEN": "x", "EGRESS_ALLOW_HOSTS": "a.test"},
	}
	for name, m := range cases {
		if _, err := load(env(m)); err != nil {
			continue
		}
		t.Errorf("%s: expected error", name)
	}
}

func TestEgressAllowedInBench(t *testing.T) {
	c, err := load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": "x", "APP_ENV": "bench", "EGRESS_ALLOW_HOSTS": "a.test, b.test"}))
	if err != nil || len(c.EgressAllowHosts) != 2 {
		t.Fatalf("got %+v, %v", c, err)
	}
}

func TestOverridesAndDebugDefault(t *testing.T) {
	c, err := load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": "x", "GO_WORKER_DB_MAX_CONNS": "7", "GO_WORKER_HEARTBEAT_SECONDS": "5"}))
	if err != nil || c.DBMaxConns != 7 || c.HeartbeatInterval != 5*time.Second || c.DebugAddr != "127.0.0.1:6060" {
		t.Fatalf("got %+v, %v", c, err)
	}
}

func TestSecretsRedacted(t *testing.T) {
	c, err := load(env(map[string]string{
		"INTERNAL_SERVICE_TOKEN": "tok-SECRET-123",
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
