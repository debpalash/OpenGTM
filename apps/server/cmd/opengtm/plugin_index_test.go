package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/index"
)

const exampleIndex = "../../../../docs/plugins/index-example"

// cleanEnv isolates a test from the developer's environment.
func cleanEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"OPENGTM_PLUGIN_INDEX", "OPENGTM_PLUGIN_DIRS", "OPENGTM_CONFIG", "CONNECTOR_SIGNATURE_POLICY",
		"OPENGTM_TRUST_STORE", "OPENGTM_PLUGIN_TRUST_STORE", "SOURCE_DATE_EPOCH"} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
}

func TestCommittedExampleIndexEndToEnd(t *testing.T) {
	cleanEnv(t)
	trust := filepath.Join(exampleIndex, "trusted-publishers.json")
	dest := filepath.Join(t.TempDir(), "plugins")
	common := []string{"--index", exampleIndex, "--trust-store", trust, "--signature-policy", "required"}

	out, err := capture(t, append([]string{"search", "firmographics"}, common...)...)
	if err != nil || !strings.Contains(out, "acme_firmographics") || strings.Contains(out, "acme_team_page") {
		t.Fatalf("search: %v\n%s", err, out)
	}
	out, err = capture(t, append([]string{"info", "acme_team_page"}, common...)...)
	if err != nil || !strings.Contains(out, "index signature trusted") || !strings.Contains(out, "https://*.acme.example") {
		t.Fatalf("info: %v\n%s", err, out)
	}
	out, err = capture(t, append([]string{"install", "acme_firmographics@0.1.0", "--destination", dest}, common...)...)
	if err != nil || !strings.Contains(out, "Installed acme_firmographics@0.1.0") || strings.Contains(out, "not signed") {
		t.Fatalf("install: %v\n%s", err, out)
	}
	// The installed plugin passes its own fixtures, offline.
	if out, err := capture(t, "test", filepath.Join(dest, "acme_firmographics")); err != nil {
		t.Fatalf("installed plugin tests: %v\n%s", err, out)
	}
	out, err = capture(t, "list", "--destination", dest, "--trust-store", trust)
	if err != nil || !strings.Contains(out, "acme_firmographics") || !strings.Contains(out, "trusted") {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if _, err := capture(t, append([]string{"install", "acme_firmographics", "--destination", dest}, common...)...); err == nil {
		t.Fatal("second install must demand --replace or update")
	}
	out, err = capture(t, append([]string{"update", "--dry-run", "--destination", dest}, common...)...)
	if err != nil || !strings.Contains(out, "up-to-date") {
		t.Fatalf("update: %v\n%s", err, out)
	}
	// Without the example's trust store the demo key is untrusted: both the
	// index signature and the bundle fail, nothing is installed.
	other := filepath.Join(t.TempDir(), "empty.json")
	os.WriteFile(other, []byte(`{"version":1,"keys":[]}`), 0o644)
	dest2 := filepath.Join(t.TempDir(), "plugins")
	if _, err := capture(t, "install", "acme_firmographics", "--index", exampleIndex, "--trust-store", other, "--destination", dest2); err == nil {
		t.Fatal("an index signed by an untrusted key must be refused")
	}
	if entries, _ := os.ReadDir(dest2); len(entries) != 0 {
		t.Fatalf("a refused install left files: %v", entries)
	}
	// Offline verification of the whole mirror.
	out, err = capture(t, "index", "verify", exampleIndex, "--deep", "--trust-store", trust, "--signature-policy", "required")
	if err != nil || !strings.Contains(out, "signature trusted, 2 plugin(s), 2 release(s), 0 problem(s)") {
		t.Fatalf("index verify: %v\n%s", err, out)
	}
	if err := func() error { _, err := capture(t, "remove", "acme_firmographics", "--destination", dest); return err }(); err != nil {
		t.Fatal(err)
	}
	if out, _ := capture(t, "list", "--destination", dest); !strings.Contains(out, "no plugins installed") {
		t.Fatalf("after remove: %s", out)
	}
}

func TestIndexConfigPrecedenceAndDefaults(t *testing.T) {
	cleanEnv(t)
	trust := filepath.Join(exampleIndex, "trusted-publishers.json")
	absExample, _ := filepath.Abs(exampleIndex)
	cfg := filepath.Join(t.TempDir(), "opengtm.yaml")
	os.WriteFile(cfg, []byte("plugins:\n  index_url: "+absExample+"\n  trust_store: "+filepath.Join(absExample, "trusted-publishers.json")+"\n"), 0o644)
	t.Setenv("OPENGTM_CONFIG", cfg)
	out, err := capture(t, "search", "team")
	if err != nil || !strings.Contains(out, "acme_team_page") {
		t.Fatalf("index_url from opengtm.yaml: %v\n%s", err, out)
	}
	// The environment beats the file; an empty directory has no index.
	empty := t.TempDir()
	t.Setenv("OPENGTM_PLUGIN_INDEX", empty)
	if _, err := capture(t, "search", "team"); err == nil {
		t.Fatal("OPENGTM_PLUGIN_INDEX must override opengtm.yaml")
	}
	// The flag beats the environment.
	if out, err := capture(t, "search", "team", "--index", exampleIndex, "--trust-store", trust); err != nil || !strings.Contains(out, "acme_team_page") {
		t.Fatalf("--index: %v\n%s", err, out)
	}
	// The install root defaults to the first OPENGTM_PLUGIN_DIRS entry.
	root := filepath.Join(t.TempDir(), "plugins")
	t.Setenv("OPENGTM_PLUGIN_DIRS", root+string(os.PathListSeparator)+t.TempDir())
	t.Setenv("OPENGTM_PLUGIN_INDEX", absExample)
	if out, err := capture(t, "install", "acme_team_page"); err != nil {
		t.Fatalf("install into OPENGTM_PLUGIN_DIRS: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(root, "acme_team_page", "plugin.yaml")); err != nil {
		t.Fatal("plugin not installed into the first plugin dir")
	}
	// The built-in default is a documented placeholder; failures say so.
	c, err := newIndexClient(indexFlags{index: ""})
	if err != nil {
		t.Fatal(err)
	}
	c.Source = index.DefaultURL
	if h := hint(c, errors.New("fetch plugin index x: boom")); !strings.Contains(h.Error(), "placeholder") {
		t.Fatalf("hint: %v", h)
	}
	if !strings.HasPrefix(index.DefaultURL, "https://") {
		t.Fatal("the default index must be https")
	}
}

func TestPublisherWorkflowWithBuildServeInstall(t *testing.T) {
	cleanEnv(t)
	dir := t.TempDir()
	key := filepath.Join(dir, "me.pem")
	out, err := capture(t, "keygen", "--key-id", "me-2026", "--publisher", "Me", "--out", key)
	if err != nil {
		t.Fatal(err)
	}
	var entry map[string]string
	if err := json.NewDecoder(strings.NewReader(out[strings.Index(out, "{"):])).Decode(&entry); err != nil {
		t.Fatal(err)
	}
	trust := filepath.Join(dir, "trust.json")
	store, _ := json.Marshal(map[string]any{"version": 1, "keys": []any{entry}})
	os.WriteFile(trust, store, 0o644)

	bundles := filepath.Join(dir, "site")
	os.MkdirAll(bundles, 0o755)
	for _, v := range []string{"0.1.0", "0.2.0"} {
		src := filepath.Join(dir, "src-"+v, "acme_lookup")
		if _, err := capture(t, "new", "provider", "acme_lookup", "--dir", src); err != nil {
			t.Fatal(err)
		}
		m := filepath.Join(src, "plugin.yaml")
		text, _ := os.ReadFile(m)
		os.WriteFile(m, []byte(strings.Replace(string(text), "version: 0.1.0", "version: "+v, 1)), 0o644)
		if out, err := capture(t, "sign", src, "--private-key", key, "--key-id", "me-2026"); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		if out, err := capture(t, "pack", src, "--output", filepath.Join(bundles, "acme_lookup-"+v+".ogc")); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	t.Setenv("SOURCE_DATE_EPOCH", "1790000000")
	out, err = capture(t, "index", "build", bundles, "--trust-store", trust, "--sign-key", key, "--key-id", "me-2026", "--name", "Mine")
	if err != nil || !strings.Contains(out, "1 plugin(s), 2 release(s), signed by me-2026") {
		t.Fatalf("index build: %v\n%s", err, out)
	}
	// Serve the directory like GitHub Pages would.
	srv := httptest.NewServer(http.FileServer(http.Dir(bundles)))
	defer srv.Close()
	t.Setenv("OPENGTM_PLUGIN_INDEX", srv.URL)
	t.Setenv("OPENGTM_PLUGIN_TRUST_STORE", trust)
	t.Setenv("CONNECTOR_SIGNATURE_POLICY", "required")
	dest := filepath.Join(dir, "installed")
	if out, err := capture(t, "install", "acme_lookup@0.1.0", "--destination", dest); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err = capture(t, "update", "--destination", dest)
	if err != nil || !strings.Contains(out, "updated") || !strings.Contains(out, "0.1.0 -> 0.2.0") {
		t.Fatalf("update: %v\n%s", err, out)
	}
	if out, err := capture(t, "install", "acme_lookup@0.1.0", "--replace", "--allow-downgrade", "--destination", dest); err == nil || !strings.Contains(out+err.Error(), "required signature policy") {
		t.Fatalf("a downgrade under the required policy must be refused: %v %s", err, out)
	}
	out, err = capture(t, "index", "verify", srv.URL, "--deep")
	if err != nil || !strings.Contains(out, "0 problem(s)") {
		t.Fatalf("remote verify: %v\n%s", err, out)
	}
	// An unsigned rebuild (no key) is refused by a required-policy client.
	if _, err := capture(t, "index", "build", bundles, "--trust-store", trust); err != nil {
		t.Fatal(err)
	}
	if _, err := capture(t, "update", "--destination", dest); err == nil {
		t.Fatal("unsigned index under the required policy must be refused")
	}
	// A lone --sign-key without --key-id is a usage error.
	if _, err := capture(t, "index", "build", bundles, "--sign-key", key); err == nil {
		t.Fatal("--sign-key needs --key-id")
	}
}

func TestInstallLocalBundleStillWorksOffline(t *testing.T) {
	cleanEnv(t)
	trust := filepath.Join(exampleIndex, "trusted-publishers.json")
	dest := filepath.Join(t.TempDir(), "plugins")
	out, err := capture(t, "install", filepath.Join(exampleIndex, "acme_team_page-0.1.0.ogc"), "--destination", dest, "--trust-store", trust)
	if err != nil || !strings.Contains(out, `"status": "trusted"`) {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := capture(t, "install", filepath.Join(exampleIndex, "acme_team_page-0.1.0.ogc"), "--destination", dest, "--trust-store", trust); err == nil {
		t.Fatal("reinstall without --replace must fail")
	}
	if _, err := capture(t, "install", filepath.Join(exampleIndex, "acme_team_page-0.1.0.ogc"), "--destination", dest, "--trust-store", trust, "--replace"); err != nil {
		t.Fatalf("--replace: %v", err)
	}
	if _, err := capture(t, "install", filepath.Join(exampleIndex, "acme_team_page-0.1.0.ogc"), "--destination", filepath.Join(t.TempDir(), "x"), "--trust-store", filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Fatal("an untrusted signer must be refused")
	}
}
