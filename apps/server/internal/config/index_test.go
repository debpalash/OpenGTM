package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPluginIndexURLFromFileAndEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opengtm.yaml")
	text := "plugins:\n  index_url: https://plugins.example.com/index.json\n  trust_store: /etc/opengtm/trusted.json\n"
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path, env(map[string]string{"DATABASE_URL": "postgresql://u:p@db/x"}))
	if err != nil || cfg.Plugins.IndexURL != "https://plugins.example.com/index.json" {
		t.Fatalf("file value: %+v %v", cfg.Plugins, err)
	}
	cfg, err = LoadFrom(path, env(map[string]string{"DATABASE_URL": "postgresql://u:p@db/x", "OPENGTM_PLUGIN_INDEX": "/srv/mirror"}))
	if err != nil || cfg.Plugins.IndexURL != "/srv/mirror" {
		t.Fatalf("env wins: %+v %v", cfg.Plugins, err)
	}
	// The CLI reads the same file without needing DATABASE_URL.
	p, err := LoadPlugins("", env(map[string]string{ConfigEnv: path}))
	if err != nil || p.IndexURL != "https://plugins.example.com/index.json" || p.TrustStore != "/etc/opengtm/trusted.json" {
		t.Fatalf("LoadPlugins: %+v %v", p, err)
	}
	if _, err := LoadPlugins(filepath.Join(t.TempDir(), "missing.yaml"), env(nil)); err == nil {
		t.Fatal("a missing explicit config file must fail")
	}
}
