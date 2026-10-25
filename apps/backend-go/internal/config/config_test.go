package config

import (
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
		"concurrency >64":    {"INTERNAL_SERVICE_TOKEN": "x", "WORKER_CONCURRENCY": "65"},
		"grace >300":         {"INTERNAL_SERVICE_TOKEN": "x", "SHUTDOWN_GRACE_SECONDS": "301"},
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
