package bundle

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/signing"
)

const repoRoot = "../../../../.."

// pythonCLI runs `uv run python -m apps.api.cli <args>` from the repo root.
// The test is skipped where uv or the Python environment is unavailable, or
// when OPENGTM_SKIP_PYTHON=1.
func pythonCLI(t *testing.T, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("uv", append([]string{"run", "--quiet", "python", "-m", "apps.api.cli"}, args...)...)
	root, _ := filepath.Abs(repoRoot)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python %v: %v\n%s\n%s", args, err, out, stderr.String())
	}
	return out
}

func requirePython(t *testing.T) {
	t.Helper()
	if os.Getenv("OPENGTM_SKIP_PYTHON") == "1" {
		t.Skip("OPENGTM_SKIP_PYTHON=1")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not installed; cross-language signing test needs the Python CLI")
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "apps", "api", "cli.py")); err != nil {
		t.Skip("Python sources not present")
	}
}

// A manifest that stresses canonicalization: unicode, floats, nested
// structures, YAML 1.1 booleans, comments and key order.
const crossManifest = `# cross-language signing fixture
manifest_version: "1"
name: cross_email
display_name: Cross Café ✓
tags: [email, "ünïcode", byok]
capability: email
default_confidence: 0.8
cost_per_lookup: 0.05
auth: {type: bearer, env_var: CROSS_KEY}
request:
  method: POST
  url: https://api.example.com/v1/find
  headers: {Content-Type: application/json, X-Flag: yes}
  body:
    name: "{{input.first_name}} {{input.last_name}}"
    limits: [1, 2.5, 1.0e+3, null, true]
  timeout: 20
response:
  mappings: {email: "$.data.email"}
`

func TestCrossLanguageSigningAndPackaging(t *testing.T) {
	requirePython(t)
	dir := t.TempDir()
	k := newKeyring(t, dir, "cross-2026")
	pyDir := filepath.Join(dir, "py")
	goDir := filepath.Join(dir, "go")
	pyManifest := filepath.Join(pyDir, "cross.yaml")
	goManifest := filepath.Join(goDir, "cross.yaml")
	writeFile(t, pyManifest, crossManifest)
	writeFile(t, goManifest, crossManifest)

	// Python signs -> Go verifies.
	pythonCLI(t, "connector-sign", pyManifest, "--private-key", k.pem, "--key-id", "cross-2026")
	if v := signing.VerifyManifest(pyManifest, k.trust); v.Status != signing.StatusTrusted {
		t.Fatalf("go verify of python signature: %+v", v)
	}
	// Go signs -> byte-identical envelope (Ed25519 is deterministic, so this
	// proves identical canonicalization and envelope formatting).
	if _, err := signing.SignManifest(goManifest, k.pem, "cross-2026"); err != nil {
		t.Fatal(err)
	}
	pySig, _ := os.ReadFile(pyManifest + ".sig")
	goSig, _ := os.ReadFile(goManifest + ".sig")
	if !bytes.Equal(pySig, goSig) {
		t.Fatalf("envelopes differ\n py: %s\n go: %s", pySig, goSig)
	}
	// Go signs -> Python verifies under the required policy.
	out := pythonCLI(t, "connectors", goDir, "--json", "--signature-policy", "required", "--trust-store", k.trust)
	var rep struct {
		OK         bool `json:"ok"`
		Connectors []struct {
			Signature struct {
				Status string `json:"status"`
			} `json:"signature"`
		} `json:"connectors"`
	}
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !rep.OK || len(rep.Connectors) != 1 || rep.Connectors[0].Signature.Status != "trusted" {
		t.Fatalf("python verify of go signature: %s", out)
	}

	// Python package -> Go install.
	pyBundle := filepath.Join(dir, "py.ogc")
	pythonCLI(t, "connector-package", pyManifest, "--output", pyBundle)
	res, err := Install(pyBundle, filepath.Join(dir, "go-installed"), k.trust, false)
	if err != nil || res.Signature.Status != "trusted" {
		t.Fatalf("go install of python bundle: %+v %v", res, err)
	}
	// Go package -> Python install (always into a temp destination).
	goBundle, err := PackV1(goManifest, filepath.Join(dir, "go.ogc"))
	if err != nil {
		t.Fatal(err)
	}
	pyDest := filepath.Join(dir, "py-installed")
	out = pythonCLI(t, "connector-install", goBundle, "--destination", pyDest, "--trust-store", k.trust)
	if !strings.Contains(string(out), `"status": "trusted"`) {
		t.Fatalf("python install of go bundle: %s", out)
	}
	installed, err := os.ReadFile(filepath.Join(pyDest, "email", "cross_email.yaml"))
	if err != nil || string(installed) != crossManifest {
		t.Fatalf("python-installed manifest: %v", err)
	}
	// Tampering after signing is detected by both sides.
	writeFile(t, goManifest, strings.Replace(crossManifest, "$.data.email", "$.data.other", 1))
	if v := signing.VerifyManifest(goManifest, k.trust); v.Status != signing.StatusInvalid || v.Error != "manifest digest mismatch" {
		t.Fatalf("go tamper detection: %+v", v)
	}
	cmd := exec.Command("uv", "run", "--quiet", "python", "-m", "apps.api.cli", "connectors", goDir, "--json", "--trust-store", k.trust)
	cmd.Dir, _ = filepath.Abs(repoRoot)
	out, _ = cmd.Output()
	if !strings.Contains(string(out), "manifest digest mismatch") {
		t.Fatalf("python tamper detection: %s", out)
	}
}
