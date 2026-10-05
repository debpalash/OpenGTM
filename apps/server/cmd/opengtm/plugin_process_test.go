//go:build unix

package main

import (
	"context"

	"github.com/debpalash/OpenGTM/apps/server/internal/config"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// useRepoSDK points process plugins at the in-repo SDK, as a contributor
// without a pip install would.
func useRepoSDK(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	sdk, err := filepath.Abs("../../../../packages/sdk-python/src")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENGTM_PLUGIN_PYTHONPATH", sdk)
	t.Setenv("OPENGTM_PLUGIN_STATE_DIR", t.TempDir())
}

func TestPluginTestRunsProcessPluginFixtures(t *testing.T) {
	useRepoSDK(t)
	out, err := capture(t, "test", filepath.Join(examplesDir, "python-provider"))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, c := range []string{"PASS basic", "PASS not_found", "PASS vendor_error", "PASS missing_secret", "4 passed, 0 failed"} {
		if !strings.Contains(out, c) {
			t.Fatalf("missing %q in:\n%s", c, out)
		}
	}
}

func TestDoctorChecksProcessPluginPrerequisitesOnlyWhenNeeded(t *testing.T) {
	useRepoSDK(t)
	ctx := context.Background()
	with := config.Config{Plugins: config.Plugins{Dirs: []string{filepath.Join(examplesDir, "python-provider")}, SignaturePolicy: "optional"}}
	checks := processChecks(ctx, with)
	names := map[string]bool{}
	for _, c := range checks {
		names[c.Name] = true
		if !c.OK {
			t.Errorf("%s: %s", c.Name, c.Detail)
		}
	}
	for _, want := range []string{"python interpreter", "opengtm-sdk", "plugin state dir"} {
		if !names[want] {
			t.Errorf("missing check %q in %+v", want, checks)
		}
	}
	// The SDK is not importable: doctor says how to fix it.
	t.Setenv("OPENGTM_PLUGIN_PYTHONPATH", t.TempDir())
	for _, c := range processChecks(ctx, with) {
		if c.Name == "opengtm-sdk" && (c.OK || !strings.Contains(c.Detail, "OPENGTM_PLUGIN_PYTHONPATH")) {
			t.Errorf("sdk check: %+v", c)
		}
	}
	// Without a process plugin installed there is nothing to check.
	without := config.Config{Plugins: config.Plugins{Dirs: []string{filepath.Join(examplesDir, "declarative-provider")}, SignaturePolicy: "optional"}}
	if got := processChecks(ctx, without); len(got) != 0 {
		t.Errorf("unexpected checks: %+v", got)
	}
}

func TestPluginTestFailsWhenAProcessPluginMisbehaves(t *testing.T) {
	useRepoSDK(t)
	dir := filepath.Join(t.TempDir(), "broken")
	if _, err := capture(t, "new", "provider", "broken_one", "--runtime", "process", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	// An expectation the plugin does not meet is reported, with the case name.
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "basic", "case.yaml"), []byte(`description: wrong
input: { domain: acme.example }
secrets: { BROKEN_ONE_API_KEY: k }
http:
  - { method: GET, url: "https://api.example.com/v1/lookup?domain=acme.example", status: 200, body: '{"data": {"employees": 1}}' }
expect:
  fields: { company_size: 999 }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := capture(t, "test", dir)
	if err == nil || !strings.Contains(out, "FAIL basic") || !strings.Contains(out, "company_size") {
		t.Fatalf("a wrong expectation must fail: %v\n%s", err, out)
	}
	// A request the fixture did not record fails the case instead of reaching the network.
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "basic", "case.yaml"), []byte(`description: unrecorded
input: { domain: acme.example }
secrets: { BROKEN_ONE_API_KEY: k }
http: []
expect:
  fields: {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = capture(t, "test", dir)
	if err == nil || !strings.Contains(out, "unrecorded request") {
		t.Fatalf("an unrecorded request must fail the case: %v\n%s", err, out)
	}
}

func TestPluginNewProcessScaffoldsPassTheirOwnTests(t *testing.T) {
	useRepoSDK(t)
	for _, kind := range []string{"provider", "scraper", "function", "tool"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), kind+"_plugin")
			out, err := capture(t, "new", kind, kind+"_plugin", "--runtime", "process", "--dir", dir)
			if err != nil || !strings.Contains(out, "opengtm plugin test") {
				t.Fatalf("new: %v\n%s", err, out)
			}
			out, err = capture(t, "test", dir)
			if err != nil || !strings.Contains(out, "PASS basic") {
				t.Fatalf("scaffolded %s must pass its own fixture: %v\n%s", kind, err, out)
			}
			if out, err := capture(t, "validate", dir); err != nil {
				t.Fatalf("validate: %v\n%s", err, out)
			}
		})
	}
	if _, err := capture(t, "new", "signal", "some_signal", "--runtime", "process", "--dir", t.TempDir()+"/x"); err == nil {
		t.Fatal("no process scaffold for signal plugins")
	}
}
