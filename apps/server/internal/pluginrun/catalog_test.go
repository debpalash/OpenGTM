package pluginrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
)

const scraperYAML = `manifest_version: "2"
name: %s
kind: scraper
runtime: declarative
version: 0.1.0
capabilities:
  network: ["https://*.example.com"]
inputs: { domain: string }
outputs: person
scrape:
  start: "https://{{input.domain}}/team"
  items: "css:.member"
  fields:
    full_name: "css:.name"
`

func writePlugin(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(scraperYAML, "%s", name, 1)
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogDiscoveryRules(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, filepath.Join(root, "team"), "team_page")
	writePlugin(t, filepath.Join(root, "vendor", "nested"), "nested_page")
	writePlugin(t, filepath.Join(root, "examples", "demo"), "demo_page")         // opt-in only
	writePlugin(t, filepath.Join(root, ".cache", "stale"), "stale_page")         // hidden
	writePlugin(t, filepath.Join(root, "team", "fixtures", "x"), "fixture_page") // below a plugin
	writePlugin(t, filepath.Join(root, "copy"), "team_page")                     // duplicate name

	c := LoadCatalog(config.Plugins{Dirs: []string{root}, SignaturePolicy: "optional"})
	var names []string
	for _, e := range c.List() {
		names = append(names, e.Plugin.Name)
	}
	if strings.Join(names, ",") != "nested_page,team_page" {
		t.Fatalf("loaded %v; errors %+v", names, c.Errors)
	}
	if len(c.Errors) != 1 || !strings.Contains(c.Errors[0].Error, "duplicate plugin name team_page") {
		t.Fatalf("errors = %+v", c.Errors)
	}

	// An examples directory named as the root itself is searched.
	ex := LoadCatalog(config.Plugins{Dirs: []string{filepath.Join(root, "examples")}, SignaturePolicy: "optional"})
	if _, ok := ex.Get("demo_page"); !ok {
		t.Fatalf("examples root not searched: %+v", ex.Errors)
	}
}

func TestCatalogRequiredSignaturesRejectUnsigned(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, filepath.Join(root, "team"), "team_page")
	c := LoadCatalog(config.Plugins{Dirs: []string{root}, SignaturePolicy: "required"})
	if len(c.List()) != 0 || len(c.Errors) != 1 || !strings.Contains(c.Errors[0].Error, "signature unsigned") {
		t.Fatalf("required policy loaded an unsigned plugin: %+v %+v", c.List(), c.Errors)
	}
}

func TestCatalogLoadsV1Connectors(t *testing.T) {
	dir := filepath.Join(dbtest.RepoRoot(t), "apps", "api", "services", "leadgen", "enrichment", "declarative", "manifests")
	c := LoadCatalog(config.Plugins{ConnectorDirs: []string{dir}, SignaturePolicy: "optional"})
	if len(c.Errors) != 0 || len(c.List()) < 4 {
		t.Fatalf("connectors: %d loaded, errors %+v", len(c.List()), c.Errors)
	}
	for _, e := range c.List() {
		if e.Source != "connector" || e.Plugin.Kind != "provider" || e.Plugin.Runtime != "declarative" {
			t.Errorf("%s: source=%s kind=%s runtime=%s", e.Plugin.Name, e.Source, e.Plugin.Kind, e.Plugin.Runtime)
		}
	}
}
