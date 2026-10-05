// Package config loads the opengtm runtime configuration.
//
// Environment variable names match the legacy Python settings so one .env file
// drives both stacks during the migration. An optional opengtm.yaml supplies
// the same values; the environment always wins so container overrides keep
// working. Everything is validated once at load so a bad value fails startup
// rather than surfacing as a confusing runtime error.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ConfigEnv names the environment variable that points at opengtm.yaml when
// no --config flag is given.
const ConfigEnv = "OPENGTM_CONFIG"

// Config is the validated runtime configuration shared by every role.
type Config struct {
	// DatabaseURL is a libpq-style postgres:// URL; SQLAlchemy driver suffixes
	// are already stripped.
	DatabaseURL  string
	Listen       string
	LegacyAPIURL string
	WebDir       string
	LogLevel     slog.Level
	Worker       Worker
	Plugins      Plugins
	Automations  Automations
}

// Automations mirrors the Python AUTOMATIONS_* switch that job executors
// honour: when disabled (the default) nothing may enqueue trigger_eval work,
// exactly as in apps/api/services/automations/events.py.
type Automations struct {
	Enabled bool
}

// Plugins locates installed plugins and sets how they are trusted.
type Plugins struct {
	// Dirs are searched for v2 plugins (directories holding plugin.yaml).
	Dirs []string
	// ConnectorDirs hold v1 connector manifests, validated exactly as the
	// Python connector registry does.
	ConnectorDirs []string
	// SignaturePolicy is "optional" or "required"; shared with Python
	// through CONNECTOR_SIGNATURE_POLICY.
	SignaturePolicy string
	// TrustStore is the trusted-publishers JSON file.
	TrustStore string
	// EgressProxy is an optional HTTP proxy for plugin traffic.
	EgressProxy string
}

// Worker mirrors the Python queue knobs, including their clamping ranges, so
// a mixed fleet behaves the same under one configuration.
type Worker struct {
	Concurrency           int
	MaxActivePerWorkspace int
	ShutdownGrace         time.Duration
}

// file is the opengtm.yaml shape. Pointers distinguish "absent" from zero so
// a file can set max_active_per_workspace: 0 (disable the cap) explicitly.
type file struct {
	DatabaseURL  *string `yaml:"database_url"`
	Listen       *string `yaml:"listen"`
	LegacyAPIURL *string `yaml:"legacy_api_url"`
	WebDir       *string `yaml:"web_dir"`
	LogLevel     *string `yaml:"log_level"`
	Plugins      struct {
		Dirs            []string `yaml:"dirs"`
		ConnectorDirs   []string `yaml:"connector_dirs"`
		SignaturePolicy *string  `yaml:"signature_policy"`
		TrustStore      *string  `yaml:"trust_store"`
		EgressProxy     *string  `yaml:"egress_proxy"`
	} `yaml:"plugins"`
	Automations struct {
		Enabled *bool `yaml:"enabled"`
	} `yaml:"automations"`
	Worker struct {
		Concurrency           *int `yaml:"concurrency"`
		MaxActivePerWorkspace *int `yaml:"max_active_per_workspace"`
		ShutdownGraceSeconds  *int `yaml:"shutdown_grace_seconds"`
	} `yaml:"worker"`
}

// Load reads path (or $OPENGTM_CONFIG when path is empty) and the process
// environment.
func Load(path string) (Config, error) {
	if path == "" {
		path = os.Getenv(ConfigEnv)
	}
	return LoadFrom(path, os.LookupEnv)
}

// LoadFrom is Load with an injectable environment, for tests.
func LoadFrom(path string, lookup func(string) (string, bool)) (Config, error) {
	var f file
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("config: read %s: %w", path, err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		// Unknown keys are almost always typos; silently ignoring them would
		// leave the operator running with a default they did not intend.
		dec.KnownFields(true)
		if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
			return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}

	var errs []error
	str := func(env string, fromFile *string, def string) string {
		if v, ok := lookup(env); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		if fromFile != nil {
			return strings.TrimSpace(*fromFile)
		}
		return def
	}
	num := func(env string, fromFile *int, def, lo, hi int) int {
		v := def
		if fromFile != nil {
			v = *fromFile
		}
		if raw, ok := lookup(env); ok && strings.TrimSpace(raw) != "" {
			n, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil {
				errs = append(errs, fmt.Errorf("%s=%q is not an integer", env, raw))
				return def
			}
			v = n
		}
		return min(hi, max(lo, v))
	}

	flag := func(env string, fromFile *bool, def bool) bool {
		v := def
		if fromFile != nil {
			v = *fromFile
		}
		if raw, ok := lookup(env); ok && strings.TrimSpace(raw) != "" {
			b, err := ParseBool(raw)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s=%q is not a boolean (true/false, 1/0, yes/no, on/off)", env, raw))
				return def
			}
			v = b
		}
		return v
	}

	cfg := Config{
		Automations:  Automations{Enabled: flag("AUTOMATIONS_ENABLED", f.Automations.Enabled, false)},
		Listen:       str("OPENGTM_LISTEN", f.Listen, ":8080"),
		LegacyAPIURL: str("OPENGTM_LEGACY_API_URL", f.LegacyAPIURL, "http://127.0.0.1:8000"),
		WebDir:       str("OPENGTM_WEB_DIR", f.WebDir, ""),
		Worker: Worker{
			Concurrency:           num("WORKER_CONCURRENCY", f.Worker.Concurrency, 1, 1, 64),
			MaxActivePerWorkspace: num("WORKER_MAX_ACTIVE_PER_WORKSPACE", f.Worker.MaxActivePerWorkspace, 2, 0, 64),
			ShutdownGrace: time.Duration(num("WORKER_SHUTDOWN_GRACE_SECONDS",
				f.Worker.ShutdownGraceSeconds, 30, 0, 300)) * time.Second,
		},
	}

	paths := func(env string, fromFile []string) []string {
		if v, ok := lookup(env); ok && strings.TrimSpace(v) != "" {
			var out []string
			for _, p := range filepath.SplitList(v) {
				if p = strings.TrimSpace(p); p != "" {
					out = append(out, p)
				}
			}
			return out
		}
		return fromFile
	}
	cfg.Plugins = Plugins{
		Dirs:            paths("OPENGTM_PLUGIN_DIRS", f.Plugins.Dirs),
		ConnectorDirs:   paths("OPENGTM_CONNECTOR_DIRS", f.Plugins.ConnectorDirs),
		SignaturePolicy: strings.ToLower(str("CONNECTOR_SIGNATURE_POLICY", f.Plugins.SignaturePolicy, "optional")),
		TrustStore:      str("OPENGTM_PLUGIN_TRUST_STORE", f.Plugins.TrustStore, ""),
		EgressProxy:     str("OPENGTM_EGRESS_PROXY", f.Plugins.EgressProxy, ""),
	}
	if p := cfg.Plugins.SignaturePolicy; p != "optional" && p != "required" {
		errs = append(errs, fmt.Errorf("CONNECTOR_SIGNATURE_POLICY=%q must be optional or required", p))
	}
	for _, dir := range append(append([]string{}, cfg.Plugins.Dirs...), cfg.Plugins.ConnectorDirs...) {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			errs = append(errs, fmt.Errorf("plugin directory %q is not a readable directory", dir))
		}
	}

	dbURL, err := NormalizeDatabaseURL(str("DATABASE_URL", f.DatabaseURL, ""))
	if err != nil {
		errs = append(errs, err)
	}
	cfg.DatabaseURL = dbURL

	if level, err := parseLevel(str("LOG_LEVEL", f.LogLevel, "info")); err != nil {
		errs = append(errs, err)
	} else {
		cfg.LogLevel = level
	}
	if _, _, err := net.SplitHostPort(cfg.Listen); err != nil {
		errs = append(errs, fmt.Errorf("OPENGTM_LISTEN=%q must be host:port (e.g. :8080): %v", cfg.Listen, err))
	}
	if u, err := url.Parse(cfg.LegacyAPIURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("OPENGTM_LEGACY_API_URL=%q must be an absolute http(s) URL", cfg.LegacyAPIURL))
	}
	if cfg.WebDir != "" {
		if st, err := os.Stat(cfg.WebDir); err != nil || !st.IsDir() {
			errs = append(errs, fmt.Errorf("OPENGTM_WEB_DIR=%q is not a readable directory", cfg.WebDir))
		}
	}
	if len(errs) > 0 {
		return Config{}, fmt.Errorf("config: %w", errors.Join(errs...))
	}
	return cfg, nil
}

// NormalizeDatabaseURL converts the SQLAlchemy URL the Python stack uses
// (postgresql+psycopg://...) into one pgx understands, and rejects SQLite:
// the Go roles depend on row locking, advisory locks, RLS and LISTEN/NOTIFY.
func NormalizeDatabaseURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("DATABASE_URL is required (a postgresql:// URL)")
	}
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return "", errors.New("DATABASE_URL must be a URL such as postgresql://user:pass@host:5432/db")
	}
	base, _, _ := strings.Cut(strings.ToLower(scheme), "+")
	switch base {
	case "postgresql", "postgres":
		return "postgresql://" + rest, nil
	case "sqlite":
		return "", errors.New("DATABASE_URL points at SQLite; the Go server requires PostgreSQL " +
			"(SQLite remains supported only by the legacy Python dev stack)")
	default:
		return "", fmt.Errorf("DATABASE_URL scheme %q is not supported; use postgresql://", scheme)
	}
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error", "critical", "fatal":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("LOG_LEVEL=%q must be one of debug, info, warn, error", s)
}

// ParseBool accepts the boolean spellings pydantic-settings accepts for the
// Python settings, so one .env value means the same to both stacks.
func ParseBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "t", "yes", "y", "on":
		return true, nil
	case "0", "false", "f", "no", "n", "off":
		return false, nil
	}
	return false, fmt.Errorf("%q is not a boolean", raw)
}
