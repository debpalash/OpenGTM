package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(kv map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := kv[k]
		return v, ok
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := LoadFrom("", env(map[string]string{
		"DATABASE_URL": "postgresql+psycopg://u:p@db:5432/yupcha",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != "postgresql://u:p@db:5432/yupcha" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.Listen != ":8080" || cfg.LegacyAPIURL != "http://127.0.0.1:8000" || cfg.WebDir != "" {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	want := Worker{Concurrency: 1, MaxActivePerWorkspace: 2, ShutdownGrace: 30 * time.Second}
	if cfg.Worker != want {
		t.Errorf("Worker = %+v, want %+v", cfg.Worker, want)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v", cfg.LogLevel)
	}
}

func TestClampingMatchesPython(t *testing.T) {
	cfg, err := LoadFrom("", env(map[string]string{
		"DATABASE_URL":                    "postgres://h/db",
		"WORKER_CONCURRENCY":              "999",
		"WORKER_MAX_ACTIVE_PER_WORKSPACE": "-4",
		"WORKER_SHUTDOWN_GRACE_SECONDS":   "100000",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Worker{Concurrency: 64, MaxActivePerWorkspace: 0, ShutdownGrace: 300 * time.Second}
	if cfg.Worker != want {
		t.Errorf("Worker = %+v, want %+v", cfg.Worker, want)
	}
	cfg, err = LoadFrom("", env(map[string]string{"DATABASE_URL": "postgres://h/db", "WORKER_CONCURRENCY": "0"}))
	if err != nil || cfg.Worker.Concurrency != 1 {
		t.Errorf("concurrency 0 should clamp to 1, got %d (%v)", cfg.Worker.Concurrency, err)
	}
}

func TestFileThenEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opengtm.yaml")
	body := `
database_url: postgresql+psycopg://file/db
listen: "127.0.0.1:9090"
log_level: debug
web_dir: ` + dir + `
worker:
  concurrency: 4
  max_active_per_workspace: 0
  shutdown_grace_seconds: 5
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path, env(map[string]string{"WORKER_CONCURRENCY": "8"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != "postgresql://file/db" || cfg.Listen != "127.0.0.1:9090" || cfg.WebDir != dir {
		t.Errorf("file values not applied: %+v", cfg)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v", cfg.LogLevel)
	}
	want := Worker{Concurrency: 8, MaxActivePerWorkspace: 0, ShutdownGrace: 5 * time.Second}
	if cfg.Worker != want {
		t.Errorf("Worker = %+v, want %+v (env must override file, explicit 0 kept)", cfg.Worker, want)
	}
}

func TestValidationErrors(t *testing.T) {
	dir := t.TempDir()
	unknown := filepath.Join(dir, "bad.yaml")
	os.WriteFile(unknown, []byte("databse_url: x\n"), 0o600)

	cases := []struct {
		name string
		path string
		env  map[string]string
		want string
	}{
		{"missing db", "", map[string]string{}, "DATABASE_URL is required"},
		{"sqlite", "", map[string]string{"DATABASE_URL": "sqlite:///data/app.db"}, "requires PostgreSQL"},
		{"mysql", "", map[string]string{"DATABASE_URL": "mysql://h/db"}, "not supported"},
		{"bad int", "", map[string]string{"DATABASE_URL": "postgres://h/db", "WORKER_CONCURRENCY": "lots"}, "not an integer"},
		{"bad listen", "", map[string]string{"DATABASE_URL": "postgres://h/db", "OPENGTM_LISTEN": "8080"}, "OPENGTM_LISTEN"},
		{"bad legacy", "", map[string]string{"DATABASE_URL": "postgres://h/db", "OPENGTM_LEGACY_API_URL": "ftp://x"}, "OPENGTM_LEGACY_API_URL"},
		{"bad web dir", "", map[string]string{"DATABASE_URL": "postgres://h/db", "OPENGTM_WEB_DIR": filepath.Join(dir, "nope")}, "OPENGTM_WEB_DIR"},
		{"bad level", "", map[string]string{"DATABASE_URL": "postgres://h/db", "LOG_LEVEL": "loud"}, "LOG_LEVEL"},
		{"unknown key", unknown, map[string]string{"DATABASE_URL": "postgres://h/db"}, "databse_url"},
		{"missing file", filepath.Join(dir, "absent.yaml"), map[string]string{}, "absent.yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadFrom(tc.path, env(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestLoadUsesConfigEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opengtm.yaml")
	os.WriteFile(path, []byte("database_url: postgres://from-env-file/db\n"), 0o600)
	t.Setenv(ConfigEnv, path)
	t.Setenv("DATABASE_URL", "")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != "postgresql://from-env-file/db" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
}
