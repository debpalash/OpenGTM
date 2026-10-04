package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a goroutine-safe buffer for the dev loop test.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestPluginDevRerunsOnChange(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dev_lookup")
	if _, err := capture(t, "new", "provider", "dev_lookup", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	buf := &syncBuf{}
	stdout = buf
	defer func() { stdout = os.Stdout }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pluginDev(ctx, []string{dir, "--interval", "20ms"}) }()
	// Count finished runs (their summary line), not started ones, so the edit
	// below never lands while the first run is still in progress.
	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for strings.Count(buf.String(), " passed, ") < n {
			if time.Now().After(deadline) {
				t.Fatalf("dev loop did not rerun:\n%s", buf.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor(1)
	time.Sleep(50 * time.Millisecond)
	os.WriteFile(filepath.Join(dir, "fixtures", "basic", "response.json"), []byte(`{"data": {"employees": 7}}`), 0o644)
	waitFor(2)
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "PASS basic") || !strings.Contains(buf.String(), "FAIL basic") {
		t.Fatalf("expected a pass then a fail after the edit:\n%s", buf.String())
	}
}

const examplesDir = "../../../../plugins/examples"

func capture(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	stdout, stderr = &out, &errOut
	defer func() { stdout, stderr = os.Stdout, os.Stderr }()
	err := runPlugin(context.Background(), args)
	return out.String() + errOut.String(), err
}

func TestPluginNewAndTestProvider(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acme_lookup")
	if out, err := capture(t, "new", "provider", "acme_lookup", "--dir", dir); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err := capture(t, "test", dir)
	if err != nil || !strings.Contains(out, "PASS basic") {
		t.Fatalf("scaffolded provider test must pass: %v\n%s", err, out)
	}
	if _, err := capture(t, "new", "provider", "acme_lookup", "--dir", dir); err == nil {
		t.Fatal("non-empty target must be refused")
	}
	if _, err := capture(t, "new", "tool", "some_tool"); err == nil {
		t.Fatal("declarative tools are not supported")
	}
	if _, err := capture(t, "new", "provider", "Bad-Name"); err == nil {
		t.Fatal("invalid names must be refused")
	}
}

func TestPluginScraperWithoutKernelFailsClearly(t *testing.T) {
	saved := kernelExtractor
	kernelExtractor = nil
	defer func() { kernelExtractor = saved }()
	out, err := capture(t, "test", filepath.Join(examplesDir, "declarative-scraper"))
	if err == nil || !strings.Contains(out, "extraction kernel") {
		t.Fatalf("want a clear kernel error, got %v\n%s", err, out)
	}
}

func TestPluginTestExamples(t *testing.T) {
	for _, ex := range []string{"declarative-provider", "declarative-scraper", "wasm-echo-provider"} {
		out, err := capture(t, "test", filepath.Join(examplesDir, ex), "--json")
		if err != nil {
			t.Fatalf("%s: %v\n%s", ex, err, out)
		}
		var res struct {
			Pass bool `json:"pass"`
		}
		if json.NewDecoder(strings.NewReader(out[strings.Index(out, "{"):])).Decode(&res); !res.Pass {
			t.Fatalf("%s: %s", ex, out)
		}
	}
}

func TestPluginValidate(t *testing.T) {
	out, err := capture(t, "validate", filepath.Join(examplesDir, "declarative-provider"), filepath.Join(examplesDir, "declarative-scraper"), filepath.Join(examplesDir, "wasm-echo-provider"), "--json")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var rep validateReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil || !rep.OK || rep.Count != 3 {
		t.Fatalf("report %s (%v)", out, err)
	}
	connectors := "../../../api/services/leadgen/enrichment/declarative/manifests"
	out, err = capture(t, "validate", connectors)
	if err != nil || !strings.HasPrefix(out, "PASS: 4 compatible connector(s)") {
		t.Fatalf("v1 connectors: %v\n%s", err, out)
	}
	if _, err := capture(t, "validate", filepath.Join(examplesDir, "declarative-provider"), "--signature-policy", "required"); err == nil {
		t.Fatal("unsigned plugin must fail under the required policy")
	}
}

func TestPluginKeygenSignVerifyPackInstall(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "me.pem")
	out, err := capture(t, "keygen", "--key-id", "me-2026", "--out", key)
	if err != nil {
		t.Fatal(err)
	}
	var entry map[string]string
	if err := json.NewDecoder(strings.NewReader(out[strings.Index(out, "{"):])).Decode(&entry); err != nil || entry["key_id"] != "me-2026" {
		t.Fatalf("keygen output %s", out)
	}
	trust := filepath.Join(dir, "trust.json")
	store, _ := json.Marshal(map[string]any{"version": 1, "keys": []any{entry}})
	os.WriteFile(trust, store, 0o644)

	plugin := filepath.Join(dir, "acme_firmographics")
	if _, err := capture(t, "new", "provider", "acme_firmographics", "--dir", plugin); err != nil {
		t.Fatal(err)
	}
	if out, err := capture(t, "sign", plugin, "--private-key", key, "--key-id", "me-2026"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if out, err := capture(t, "verify", plugin, "--trust-store", trust); err != nil || !strings.Contains(out, `"trusted"`) {
		t.Fatalf("verify: %v %s", err, out)
	}
	if out, err := capture(t, "validate", plugin, "--signature-policy", "required", "--trust-store", trust); err != nil {
		t.Fatalf("validate signed: %v %s", err, out)
	}
	bundlePath := filepath.Join(dir, "acme.ogc")
	if out, err := capture(t, "pack", plugin, "--output", bundlePath); err != nil {
		t.Fatalf("pack: %v %s", err, out)
	}
	dest := filepath.Join(dir, "installed")
	if out, err := capture(t, "install", bundlePath, "--destination", dest, "--trust-store", trust); err != nil || !strings.Contains(out, `"status": "trusted"`) {
		t.Fatalf("install: %v %s", err, out)
	}
	if out, err := capture(t, "test", filepath.Join(dest, "acme_firmographics")); err != nil {
		t.Fatalf("installed plugin tests: %v %s", err, out)
	}
	// Tampering breaks verification.
	m := filepath.Join(plugin, "plugin.yaml")
	data, _ := os.ReadFile(m)
	os.WriteFile(m, bytes.Replace(data, []byte("0.7"), []byte("0.9"), 1), 0o644)
	if _, err := capture(t, "verify", plugin, "--trust-store", trust); err == nil {
		t.Fatal("tampered manifest must fail verification")
	}
}

func TestPluginRunAndRecordUseTheGuard(t *testing.T) {
	dir := t.TempDir()
	text := `manifest_version: "2"
name: guarded_lookup
kind: provider
runtime: declarative
version: 0.1.0
capabilities: {network: ["https://*"]}
inputs: {host: string, n: integer}
capability: email
request: {method: GET, url: "https://{{input.host}}/x?n={{input.n}}"}
response: {mappings: {email: $.email}}
`
	os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(text), 0o644)
	out, err := capture(t, "run", dir, "--input", "host=169.254.169.254", "--input", "n=3")
	if err != nil || !strings.Contains(out, "blocked_url") {
		t.Fatalf("run must go through the SSRF guard: %v\n%s", err, out)
	}
	if _, err := capture(t, "run", dir, "--input", "host=x", "--input", "n=three"); err == nil {
		t.Fatal("typed inputs must be parsed")
	}
	if _, err := capture(t, "run", dir, "--input", "host=x", "--input", "n=1", "--secret-env", "UNDECLARED"); err == nil {
		t.Fatal("undeclared --secret-env must be refused")
	}
	out, err = capture(t, "record", dir, "--input", "host=127.0.0.1", "--input", "n=1", "--case", "blocked")
	if err != nil {
		t.Fatalf("record: %v %s", err, out)
	}
	c, _ := os.ReadFile(filepath.Join(dir, "fixtures", "blocked", "case.yaml"))
	if !strings.Contains(string(c), "blocked_url") {
		t.Fatalf("recorded case: %s", c)
	}
	if out, err := capture(t, "test", dir); err != nil {
		t.Fatalf("recorded case must replay: %v\n%s", err, out)
	}
}

func TestPluginUsage(t *testing.T) {
	if _, err := capture(t, "nope"); err == nil {
		t.Fatal("unknown command must fail")
	}
	if out, _ := capture(t, "--help"); !strings.Contains(out, "keygen") {
		t.Fatal(out)
	}
}
