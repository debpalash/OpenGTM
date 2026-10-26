package manifest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

func TestLoadIndexRejectsUnknownAuthType(t *testing.T) {
	dir := t.TempDir()
	body := "name: auth_digest\ncapability: email\nauth:\n  type: digest\nrequest:\n  url: https://api.example.com/x\n"
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := manifest.LoadIndex(dir, "optional", manifest.TrustStore{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := idx["auth_digest"]; ok {
		t.Fatal("auth.type digest must be rejected (it would send the request without a credential)")
	}
}
