package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadIndexReadsManifestOnce(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "m.yaml")
	good := "name: good_one\ncapability: email\nrequest: {url: 'https://x.test'}\n"
	if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	reads := 0
	orig := readManifest
	defer func() { readManifest = orig }()
	readManifest = func(path string) ([]byte, error) {
		reads++
		b, err := orig(path)
		// swap the file after the first read: a second read would see evil bytes
		_ = os.WriteFile(p, []byte("name: evil_one\ncapability: email\nrequest: {url: 'https://e.test'}\n"), 0o600)
		return b, err
	}
	idx, err := LoadIndex(dir, "optional", TrustStore{})
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("manifest read %d times, want 1", reads)
	}
	if _, ok := idx["good_one"]; !ok || len(idx) != 1 {
		t.Fatalf("index %v: decoded bytes differ from verified bytes", idx)
	}
}

func TestVerifyRejectsSymlinkedSignature(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "m.yaml")
	_ = os.WriteFile(p, []byte("a: 1\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "real.sig"), []byte("{}"), 0o600)
	if err := os.Symlink(filepath.Join(dir, "real.sig"), p+".sig"); err != nil {
		t.Skip(err)
	}
	if st, err := Verify(p, TrustStore{}); st != SigInvalid || err == nil {
		t.Fatalf("%q %v", st, err)
	}
}
