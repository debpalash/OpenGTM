// Package config loads worker configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultWorkerConcurrency = 4
	maxWorkerConcurrency     = 64
	defaultDBMaxConns        = 10
	defaultShutdownGrace     = 30 * time.Second
	maxShutdownGrace         = 300 * time.Second
	heartbeatInterval        = 30 * time.Second
)

type Config struct {
	DatabaseURL          string
	RedisURL             string
	InternalAPIURL       string
	InternalServiceToken string
	WorkerConcurrency    int
	DBMaxConns           int
	ShutdownGrace        time.Duration
	HeartbeatInterval    time.Duration
	AppEnv               string
	EgressAllowHosts     []string
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
		HeartbeatInterval:    heartbeatInterval,
	}
	var err error
	if c.WorkerConcurrency, err = intVar(get, "WORKER_CONCURRENCY", defaultWorkerConcurrency, 1, maxWorkerConcurrency); err != nil {
		return Config{}, err
	}
	if c.DBMaxConns, err = intVar(get, "DB_MAX_CONNS", defaultDBMaxConns, 1, 1000); err != nil {
		return Config{}, err
	}
	secs, err := intVar(get, "SHUTDOWN_GRACE_SECONDS", int(defaultShutdownGrace/time.Second), 1, int(maxShutdownGrace/time.Second))
	if err != nil {
		return Config{}, err
	}
	c.ShutdownGrace = time.Duration(secs) * time.Second

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
