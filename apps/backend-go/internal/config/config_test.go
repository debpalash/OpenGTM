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

// env serves m; DATABASE_URL, SECRET_KEY and OUTREACH_PUBLIC_BASE_URL default to placeholders unless m names them
// (even as ""): the only credentials source ("local") requires the first two, the permanently-on outreach sender
// the third outside development.
func env(m map[string]string) func(string) string {
	return func(k string) string {
		if v, ok := m[k]; ok {
			return v
		}
		switch k {
		case "DATABASE_URL":
			return "postgres://x/y"
		case "SECRET_KEY":
			return "k"
		case "OUTREACH_PUBLIC_BASE_URL": // outreach sending is permanently on: required outside dev
			return "https://outreach.example.com"
		}
		return ""
	}
}

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
		"empty token":     {},
		"concurrency >64": {"INTERNAL_SERVICE_TOKEN": goodTok, "GO_WORKER_CONCURRENCY": "65"},
		"grace >300":      {"INTERNAL_SERVICE_TOKEN": goodTok, "GO_WORKER_SHUTDOWN_GRACE_SECONDS": "301"},
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

func TestEgressAllowHostsAcceptedInProduction(t *testing.T) {
	c, err := load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": goodTok, "APP_ENV": "production", "EGRESS_ALLOW_HOSTS": "reacher.internal:8080"}))
	if err != nil || len(c.EgressAllowHosts) != 1 || c.EgressAllowHosts[0] != "reacher.internal:8080" {
		t.Fatalf("got %+v, %v", c.EgressAllowHosts, err)
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

func TestConfigCredentialsSourceValidation(t *testing.T) {
	base := map[string]string{"INTERNAL_SERVICE_TOKEN": goodTok}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	c, err := load(env(base))
	if err != nil || c.CredentialsSource != "local" {
		t.Fatalf("default must be local, got %q, %v", c.CredentialsSource, err)
	}
	if c, err = load(env(with("CREDENTIALS_SOURCE", " LOCAL "))); err != nil || c.CredentialsSource != "local" {
		t.Fatalf("got %q, %v", c.CredentialsSource, err)
	}
	for name, m := range map[string]map[string]string{
		"bogus":           with("CREDENTIALS_SOURCE", "redis"),
		"http removed":    with("CREDENTIALS_SOURCE", "http"),
		"default no db":   with("DATABASE_URL", ""),
		"local no db":     with("CREDENTIALS_SOURCE", "local", "SECRET_KEY", "k", "DATABASE_URL", ""),
		"local no secret": with("CREDENTIALS_SOURCE", "local", "DATABASE_URL", "postgres://x/y", "SECRET_KEY", ""),
		"local bad prov":  with("CREDENTIALS_SOURCE", "local", "DATABASE_URL", "postgres://x/y", "SECRET_KEY", "k", "SECRETS_PROVIDER", "kms"),
		"vault no addr":   with("CREDENTIALS_SOURCE", "local", "DATABASE_URL", "postgres://x/y", "SECRET_KEY", "k", "SECRETS_PROVIDER", "vault_transit"),
	} {
		if _, err := load(env(m)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	c, err = load(env(with("CREDENTIALS_SOURCE", "local", "DATABASE_URL", "postgres://x/y", "SECRET_KEY", "k",
		"SECRETS_PROVIDER", "vault_transit", "VAULT_ADDR", "https://v:8200", "VAULT_TRANSIT_KEY", "tk", "SECRETS_MASTER_KEY", "mk", "VAULT_TOKEN", "tok")))
	if err != nil || c.CredentialsSource != "local" || c.SecretKey != "k" || c.SecretsProvider != "vault_transit" ||
		c.VaultAddr != "https://v:8200" || c.VaultTransitKey != "tk" || c.SecretsMasterKey != "mk" || c.VaultToken != "tok" {
		t.Fatalf("got %+v, %v", c, err)
	}
	// secrets never appear in the logged or printed config
	c, err = load(env(with("CREDENTIALS_SOURCE", "local", "DATABASE_URL", "postgres://x/y", "SECRET_KEY", "SK-SENTINEL",
		"SECRETS_MASTER_KEY", "MK-SENTINEL", "VAULT_TOKEN", "VT-SENTINEL")))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "config", c)
	out := buf.String() + fmt.Sprintf("%v %+v", c, c)
	for _, s := range []string{"SK-SENTINEL", "MK-SENTINEL", "VT-SENTINEL"} {
		if strings.Contains(out, s) {
			t.Fatalf("config output leaks %s", s)
		}
	}
}

func TestAutomationsEnabledStrictParse(t *testing.T) {
	if _, err := load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": goodTok, "AUTOMATIONS_ENABLED": "maybe"})); err == nil {
		t.Fatal("invalid AUTOMATIONS_ENABLED must be rejected")
	}
	c, err := load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": goodTok, "AUTOMATIONS_ENABLED": "true"}))
	if err != nil || !c.Flags.AutomationsEnabled {
		t.Fatalf("got %v, %v", c.Flags, err)
	}
}

func TestExecutorSettings(t *testing.T) {
	c, err := load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": goodTok}))
	if err != nil {
		t.Fatal(err)
	}
	e := c.Executor
	if !e.Enabled || e.RefreshExecutor("w") != "go" || e.SignalScanExecutor() != "go" ||
		e.SignalHotLeads != 50 || e.SignalWarmLeads != 30 || e.SignalPerWorkspace != 20 || e.SignalGlobal != 500 {
		t.Fatalf("defaults: %+v", e)
	}
	c, err = load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": goodTok, "GO_EXECUTOR_ENABLED": "false", "GO_EXECUTOR_WORKSPACES": " a, b ,",
		"GO_EXECUTOR_SIGNAL_SCAN": "0", "SIGNAL_SCAN_MAX_HOT_LEADS": "7", "SIGNAL_SCAN_GLOBAL_MAX_LEADS": "0"}))
	if err != nil {
		t.Fatal(err)
	}
	e = c.Executor
	if e.RefreshExecutor("a") != "go" || e.RefreshExecutor("c") != "go" || e.RefreshExecutor("") != "go" ||
		e.SignalScanExecutor() != "go" || e.SignalHotLeads != 7 || e.SignalGlobal != 0 {
		t.Fatalf("set: %+v", e)
	}
	if _, err := load(env(map[string]string{"INTERNAL_SERVICE_TOKEN": goodTok, "SIGNAL_SCAN_MAX_HOT_LEADS": "x"})); err == nil {
		t.Fatal("bad int accepted")
	}
}

func TestAPIConfigEgressAllowHosts(t *testing.T) {
	c, err := loadAPI(func(k string) string {
		switch k {
		case "EGRESS_ALLOW_HOSTS":
			return " 127.0.0.1:11434, ollama.internal ,,"
		case "DATABASE_URL":
			return "postgres://x"
		case "APP_ENV":
			return "test"
		}
		return ""
	})
	if err != nil || len(c.EgressAllowHosts) != 2 || c.EgressAllowHosts[0] != "127.0.0.1:11434" || c.EgressAllowHosts[1] != "ollama.internal" {
		t.Fatalf("got %+v, %v", c.EgressAllowHosts, err)
	}
}
