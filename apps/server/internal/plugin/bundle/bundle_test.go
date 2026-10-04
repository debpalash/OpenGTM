package bundle

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/signing"
)

const v1Text = "manifest_version: \"1\"\nname: signed_email\ncapability: email\nrequest:\n  url: https://api.example.com/find\nresponse:\n  mappings:\n    email: $.email\n"

type keyring struct {
	priv  ed25519.PrivateKey
	pem   string
	trust string
}

func newKeyring(t *testing.T, dir, keyID string) keyring {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes, err := signing.EncodePrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	k := keyring{priv: priv, pem: filepath.Join(dir, keyID+".pem"), trust: filepath.Join(dir, "trusted.json")}
	if err := os.WriteFile(k.pem, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	store, _ := json.Marshal(map[string]any{"version": 1, "keys": []any{signing.TrustEntry(keyID, "Test Publisher", pub)}})
	if err := os.WriteFile(k.trust, store, 0o644); err != nil {
		t.Fatal(err)
	}
	return k
}

func writeFile(t *testing.T, p, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readPair(t *testing.T, p string) string {
	t.Helper()
	a, _ := os.ReadFile(p)
	b, _ := os.ReadFile(p + ".sig")
	return string(a) + "|" + string(b)
}

func TestV1PackInstallReplaceAndRollback(t *testing.T) {
	dir := t.TempDir()
	k := newKeyring(t, dir, "publisher-1")
	m := filepath.Join(dir, "signed.yaml")
	writeFile(t, m, v1Text)
	if _, err := PackV1(m, ""); err == nil {
		t.Fatal("unsigned manifests must not be packaged")
	}
	if _, err := signing.SignManifest(m, k.pem, "publisher-1"); err != nil {
		t.Fatal(err)
	}
	if v := signing.VerifyManifest(m, k.trust); v.Status != "trusted" || v.Publisher != "Test Publisher" {
		t.Fatalf("verify %+v", v)
	}
	first, _ := PackV1(m, filepath.Join(dir, "first.ogc"))
	second, _ := PackV1(m, filepath.Join(dir, "second.ogc"))
	a, _ := os.ReadFile(first)
	b, _ := os.ReadFile(second)
	if !bytes.Equal(a, b) {
		t.Fatal("bundles must be deterministic")
	}
	if DefaultOutput(m) != filepath.Join(dir, "signed.ogc") {
		t.Fatal(DefaultOutput(m))
	}
	dest := filepath.Join(dir, "installed")
	res, err := Install(first, dest, k.trust, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "signed_email" || res.Capability != "email" || res.Signature.Status != "trusted" || res.Manifest != filepath.Join(dest, "email", "signed_email.yaml") {
		t.Fatalf("install %+v", res)
	}
	if _, err := Install(first, dest, k.trust, false); err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Fatalf("collision: %v", err)
	}
	if _, err := Install(first, dest, k.trust, true); err != nil {
		t.Fatal(err)
	}
	original := readPair(t, res.Manifest)

	// Upgrade whose second move fails: the previous pair is restored.
	writeFile(t, m, v1Text+"description: upgraded connector\n")
	if _, err := signing.SignManifest(m, k.pem, "publisher-1"); err != nil {
		t.Fatal(err)
	}
	upgrade, _ := PackV1(m, filepath.Join(dir, "upgrade.ogc"))
	calls := 0
	replaceHook = func(src, dst string) error {
		calls++
		if calls == 2 {
			return errors.New("simulated manifest move failure")
		}
		return os.Rename(src, dst)
	}
	_, err = Install(upgrade, dest, k.trust, true)
	replaceHook = os.Rename
	if err == nil || readPair(t, res.Manifest) != original {
		t.Fatalf("rollback failed: %v", err)
	}
	if v := signing.VerifyManifest(res.Manifest, k.trust); v.Status != "trusted" {
		t.Fatal("restored pair must still verify")
	}
	leftovers, _ := filepath.Glob(filepath.Join(dest, "email", ".signed_email.*.yaml*"))
	if len(leftovers) != 0 {
		t.Fatalf("staged files left behind: %v", leftovers)
	}
	// Fresh install failing half-way leaves nothing behind.
	calls = 0
	replaceHook = func(src, dst string) error {
		calls++
		if calls == 2 {
			return errors.New("boom")
		}
		return os.Rename(src, dst)
	}
	fresh := filepath.Join(dir, "fresh")
	_, err = Install(upgrade, fresh, k.trust, false)
	replaceHook = os.Rename
	if err == nil {
		t.Fatal("expected failure")
	}
	if left, _ := filepath.Glob(filepath.Join(fresh, "email", "*.yaml*")); len(left) != 0 {
		t.Fatalf("half-installed files: %v", left)
	}

	// Concurrent install holds the lock.
	InstallLockWait = 0
	unlock, err := lockFile(filepath.Join(dest, "email", ".signed_email.install.lock"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Install(upgrade, dest, k.trust, true)
	unlock()
	InstallLockWait = 10 * time.Second
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("lock: %v", err)
	}
}

func zipOf(t *testing.T, p string, files map[string][]byte) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		w, _ := zw.Create(name)
		w.Write(data)
	}
	zw.Close()
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestV1RejectsMalformedAndUntrustedBundles(t *testing.T) {
	dir := t.TempDir()
	k := newKeyring(t, dir, "publisher-1")
	m := filepath.Join(dir, "signed.yaml")
	writeFile(t, m, v1Text)
	signing.SignManifest(m, k.pem, "publisher-1")
	sig, _ := os.ReadFile(m + ".sig")
	untouched := filepath.Join(dir, "untouched")
	cases := map[string]map[string][]byte{
		"must contain only": {"../connector.yaml": []byte(v1Text), "connector.yaml.sig": sig},
		"1 MB file limit":   {"connector.yaml": []byte(v1Text + "#" + strings.Repeat("x", 1_000_001)), "connector.yaml.sig": sig},
	}
	for want, files := range cases {
		_, err := Install(zipOf(t, filepath.Join(dir, "bad.ogc"), files), untouched, k.trust, false)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: got %v", want, err)
		}
	}
	other := newKeyring(t, t.TempDir(), "publisher-1")
	ogc, _ := PackV1(m, filepath.Join(dir, "ok.ogc"))
	if _, err := Install(ogc, untouched, other.trust, false); err == nil || !strings.Contains(err.Error(), "signature is invalid") {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err := os.Stat(untouched); err == nil {
		if left, _ := filepath.Glob(filepath.Join(untouched, "*", "*.yaml")); len(left) != 0 {
			t.Fatal("rejected bundles must not write manifests")
		}
	}
}

func TestV2PackAndInstallWasmPlugin(t *testing.T) {
	dir := t.TempDir()
	k := newKeyring(t, dir, "publisher-2")
	src := "../../../../../plugins/examples/wasm-echo-provider"
	plug := filepath.Join(dir, "echo")
	for _, f := range []string{"plugin.yaml", "plugin.wasm"} {
		data, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(plug, f), string(data))
	}
	writeFile(t, filepath.Join(plug, "fixtures", "basic", "case.yaml"), "input: {url: https://api.github.com/x}\n")
	if _, err := Pack(plug, ""); err == nil {
		t.Fatal("unsigned v2 plugin must not pack")
	}
	if _, err := signing.SignManifest(filepath.Join(plug, "plugin.yaml"), k.pem, "publisher-2"); err != nil {
		t.Fatal(err)
	}
	ogc, err := Pack(plug, "")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(ogc) != "wasm_echo_provider-0.1.0.ogc" {
		t.Fatal(ogc)
	}
	zr, _ := zip.OpenReader(ogc)
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	zr.Close()
	if strings.Join(names, ",") != "plugin.yaml,plugin.yaml.sig,plugin.wasm,fixtures/basic/case.yaml" {
		t.Fatalf("entries %v", names)
	}
	dest := filepath.Join(dir, "plugins")
	res, err := Install(ogc, dest, k.trust, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "wasm_echo_provider" || res.Version != "0.1.0" || res.Signature.Status != "trusted" {
		t.Fatalf("%+v", res)
	}
	for _, f := range []string{"plugin.yaml", "plugin.yaml.sig", "plugin.wasm", "fixtures/basic/case.yaml"} {
		if _, err := os.Stat(filepath.Join(dest, "wasm_echo_provider", f)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Install(ogc, dest, k.trust, false); err == nil {
		t.Fatal("collision must fail without replace")
	}
	if _, err := Install(ogc, dest, k.trust, true); err != nil {
		t.Fatal(err)
	}
	// A swapped module fails the signed sha256 pin; a traversal entry is refused.
	manifestBytes, _ := os.ReadFile(filepath.Join(plug, "plugin.yaml"))
	sigBytes, _ := os.ReadFile(filepath.Join(plug, "plugin.yaml.sig"))
	evil := zipOf(t, filepath.Join(dir, "evil.ogc"), map[string][]byte{"plugin.yaml": manifestBytes, "plugin.yaml.sig": sigBytes, "plugin.wasm": []byte("\x00asm evil")})
	if _, err := Install(evil, filepath.Join(dir, "x"), k.trust, false); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("module swap: %v", err)
	}
	slip := zipOf(t, filepath.Join(dir, "slip.ogc"), map[string][]byte{"plugin.yaml": manifestBytes, "plugin.yaml.sig": sigBytes, "fixtures/../../escape": []byte("x")})
	if _, err := Install(slip, filepath.Join(dir, "y"), k.trust, false); err == nil || !strings.Contains(err.Error(), "unexpected entry") {
		t.Fatalf("zip slip: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "escape")); err == nil {
		t.Fatal("zip slip escaped")
	}
}
