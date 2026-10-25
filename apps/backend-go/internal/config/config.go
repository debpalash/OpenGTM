// Package config loads worker configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// RequiredGoSchemaRevision is the Alembic revision the Go worker requires.
// It must equal REQUIRED_GO_SCHEMA_REVISION in the Python go_executor module.
const RequiredGoSchemaRevision = "4a5162738495"

const (
	defaultWorkerConcurrency = 4
	maxWorkerConcurrency     = 64
	defaultDBMaxConns        = 10
	defaultShutdownGrace     = 30 * time.Second
	maxShutdownGrace         = 300 * time.Second
	heartbeatInterval        = 30 * time.Second

	// Shared with Python (settings.WORKER_MAX_ACTIVE_PER_WORKSPACE): default 2,
	// clamped to [0, 64]; 0 disables the per-workspace cap.
	defaultMaxActivePerWorkspace = 2
	maxMaxActivePerWorkspace     = 64
)

type Config struct {
	DatabaseURL           string
	RedisURL              string
	InternalAPIURL        string
	InternalServiceToken  string
	WorkerConcurrency     int
	MaxActivePerWorkspace int
	DBMaxConns            int
	ShutdownGrace         time.Duration
	HeartbeatInterval     time.Duration
	DebugAddr             string
	AppEnv                string
	EgressAllowHosts      []string
}

// Load reads configuration from the environment and validates it.
func Load() (Config, error) { return load(os.Getenv) }

func load(get func(string) string) (Config, error) {
	c := Config{
		DatabaseURL:          get("DATABASE_URL"),
		RedisURL:             get("REDIS_URL"),
		InternalAPIURL:       get("INTERNAL_API_URL"),
		InternalServiceToken: get("INTERNAL_SERVICE_TOKEN"),
		AppEnv:               get("APP_ENV"),
		DebugAddr:            get("GO_WORKER_DEBUG_ADDR"),
	}
	var err error
	if c.WorkerConcurrency, err = intVar(get, "GO_WORKER_CONCURRENCY", defaultWorkerConcurrency, 1, maxWorkerConcurrency); err != nil {
		return Config{}, err
	}
	if c.DBMaxConns, err = intVar(get, "GO_WORKER_DB_MAX_CONNS", defaultDBMaxConns, 1, 1000); err != nil {
		return Config{}, err
	}
	if c.MaxActivePerWorkspace, err = clampVar(get, "WORKER_MAX_ACTIVE_PER_WORKSPACE", defaultMaxActivePerWorkspace, 0, maxMaxActivePerWorkspace); err != nil {
		return Config{}, err
	}
	secs, err := intVar(get, "GO_WORKER_SHUTDOWN_GRACE_SECONDS", int(defaultShutdownGrace/time.Second), 1, int(maxShutdownGrace/time.Second))
	if err != nil {
		return Config{}, err
	}
	c.ShutdownGrace = time.Duration(secs) * time.Second

	hb, err := intVar(get, "GO_WORKER_HEARTBEAT_SECONDS", int(heartbeatInterval/time.Second), 1, 3600)
	if err != nil {
		return Config{}, err
	}
	c.HeartbeatInterval = time.Duration(hb) * time.Second
	if c.DebugAddr == "" {
		c.DebugAddr = "127.0.0.1:6060"
	}

	for _, h := range strings.Split(get("EGRESS_ALLOW_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			c.EgressAllowHosts = append(c.EgressAllowHosts, h)
		}
	}
	if c.InternalServiceToken == "" {
		return Config{}, errors.New("config: INTERNAL_SERVICE_TOKEN is required")
	}
	if len(c.EgressAllowHosts) > 0 && c.AppEnv != "test" && c.AppEnv != "bench" {
		return Config{}, fmt.Errorf("config: EGRESS_ALLOW_HOSTS is only allowed when APP_ENV is test or bench (got %q)", c.AppEnv)
	}
	return c, nil
}

// clampVar is like intVar but clamps out-of-range values (Python's
// max(lo, min(hi, n))); a non-integer is still an error.
func clampVar(get func(string) string, key string, def, lo, hi int) (int, error) {
	s := get(key)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("config: %s: %w", key, err)
	}
	return max(lo, min(hi, n)), nil
}

func intVar(get func(string) string, key string, def, lo, hi int) (int, error) {
	s := get(key)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("config: %s: %w", key, err)
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("config: %s=%d out of range [%d, %d]", key, n, lo, hi)
	}
	return n, nil
}

// redactURL masks the password component of a connection URL.
func redactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "[unparseable url redacted]"
	}
	if _, has := u.User.Password(); has {
		u.User = url.UserPassword(u.User.Username(), "xxxxx")
	}
	return u.String()
}

// redacted returns a copy safe to print or log.
func (c Config) redacted() Config {
	c.DatabaseURL = redactURL(c.DatabaseURL)
	c.RedisURL = redactURL(c.RedisURL)
	if c.InternalServiceToken != "" {
		c.InternalServiceToken = "[redacted]"
	}
	return c
}

type plain Config // no methods: avoids String recursion

// String implements fmt.Stringer with secrets redacted (covers %v and %+v).
func (c Config) String() string { return fmt.Sprintf("%+v", plain(c.redacted())) }

// LogValue implements slog.LogValuer with secrets redacted.
func (c Config) LogValue() slog.Value {
	r := c.redacted()
	return slog.GroupValue(
		slog.String("database_url", r.DatabaseURL),
		slog.String("redis_url", r.RedisURL),
		slog.String("internal_api_url", r.InternalAPIURL),
		slog.String("internal_service_token", r.InternalServiceToken),
		slog.Int("worker_concurrency", r.WorkerConcurrency),
		slog.Int("max_active_per_workspace", r.MaxActivePerWorkspace),
		slog.Int("db_max_conns", r.DBMaxConns),
		slog.Duration("shutdown_grace", r.ShutdownGrace),
		slog.Duration("heartbeat_interval", r.HeartbeatInterval),
		slog.String("debug_addr", r.DebugAddr),
		slog.String("app_env", r.AppEnv),
		slog.Any("egress_allow_hosts", r.EgressAllowHosts),
	)
}
