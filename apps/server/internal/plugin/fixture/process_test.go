//go:build unix

package fixture

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/process"
)

func processDeps(t *testing.T) Deps {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	sdk, err := filepath.Abs("../../../../../packages/sdk-python/src")
	if err != nil {
		t.Fatal(err)
	}
	return Deps{AllowPrivateForTesting: true, Process: process.Options{PythonPath: []string{sdk}, StateDir: t.TempDir()}}
}

const livePlugin = `manifest_version: "2"
name: live_python
kind: provider
runtime: process
version: 0.1.0
capabilities: {network: ["https://127.0.0.1:*"], secrets: [LIVE_KEY]}
inputs: {url: string}
process: {command: [python3, main.py]}
`

const liveMain = `from opengtm_sdk import provider, run

@provider
def lookup(ctx, url: str):
    r = ctx.fetch(url, headers={"X-Key": ctx.secret("LIVE_KEY")})
    body = r.json()
    if body.get("error"):
        from opengtm_sdk import Retryable
        raise Retryable(body["error"])
    return {"email": body["email"], "echo": body["echo"]}

if __name__ == "__main__":
    run()
`

func TestRecordAndReplayAProcessPlugin(t *testing.T) {
	deps := processDeps(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("fail") != "" {
			_, _ = io.WriteString(w, `{"error": "quota exceeded"}`)
			return
		}
		// the vendor echoes the key back, as some do in error and debug output
		_, _ = fmt.Fprintf(w, `{"email": "ada@acme.example", "echo": %q}`, r.Header.Get("X-Key"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	for name, content := range map[string]string{"plugin.yaml": livePlugin, "main.py": liveMain} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := manifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	live := egress.Options{AllowPrivateForTesting: true, DefaultRPS: 1000, TLSConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig}
	const secret = "live-secret-123"

	caseDir, out, err := Record(context.Background(), p, RecordOptions{
		Case: "live", Inputs: map[string]any{"url": srv.URL + "/lookup?d=acme.example"},
		Secrets: map[string]string{"LIVE_KEY": secret}, Deps: deps, ClientOptions: live,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Fields["email"] != "ada@acme.example" {
		t.Fatalf("live outcome: %+v", out)
	}
	var all strings.Builder
	files, _ := os.ReadDir(caseDir)
	for _, f := range files {
		b, _ := os.ReadFile(filepath.Join(caseDir, f.Name()))
		all.Write(b)
	}
	if strings.Contains(all.String(), secret) {
		t.Fatalf("the recorded fixture contains the secret:\n%s", all.String())
	}
	// A recorded failure the plugin reported is an expectation, not an error.
	if _, _, err := Record(context.Background(), p, RecordOptions{
		Case: "quota", Inputs: map[string]any{"url": srv.URL + "/lookup?fail=1"},
		Secrets: map[string]string{"LIVE_KEY": secret}, Deps: deps, ClientOptions: live,
	}); err != nil {
		t.Fatal(err)
	}

	for _, r := range runAll(t, dir, deps) {
		if !r.Pass {
			t.Errorf("replay of %s: %v", r.Case, r.Problems)
		}
	}
	quota, _ := os.ReadFile(filepath.Join(dir, "fixtures", "quota", "case.yaml"))
	if !strings.Contains(string(quota), "upstream_error: quota exceeded") {
		t.Fatalf("failure not recorded as an expectation:\n%s", quota)
	}
}

func TestProcessFailuresOfTheMachineryFailTheCase(t *testing.T) {
	deps := processDeps(t)
	dir := t.TempDir()
	crash := `import os, signal
from opengtm_sdk import provider, run

@provider
def lookup(ctx, url: str):
    os.kill(os.getpid(), signal.SIGSEGV)

if __name__ == "__main__":
    run()
`
	for name, content := range map[string]string{"plugin.yaml": livePlugin, "main.py": crash} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := manifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := &Case{Name: "crash", Input: map[string]any{"url": "https://127.0.0.1/x"}, Expect: Expect{Match: "exact", Error: "plugin_crashed", Fields: map[string]any{}}}
	r := RunCase(context.Background(), p, c, deps)
	if r.Pass || !strings.Contains(fmt.Sprint(r.Problems), "plugin_crashed") {
		t.Fatalf("a crashing plugin must not pass by expecting its crash: %+v", r)
	}
}
