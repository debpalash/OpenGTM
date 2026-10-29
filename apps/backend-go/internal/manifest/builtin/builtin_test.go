package builtin

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testutil"
)

const legacyManifests = "apps/api/services/leadgen/enrichment/declarative/manifests"

func TestEmbeddedManifestsMatchLegacy(t *testing.T) {
	root := testutil.LegacyPath(t, filepath.FromSlash(legacyManifests))
	seen := map[string]bool{}
	err := fs.WalkDir(os.DirFS(root), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".pyc") || strings.Contains(p, "__pycache__") {
			return err
		}
		want, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		got, err := files.ReadFile("manifests/" + p)
		if err != nil {
			t.Errorf("legacy file %s is not embedded", p)
			return nil
		}
		seen["manifests/"+p] = true
		if !bytes.Equal(got, want) {
			t.Errorf("embedded %s differs from legacy", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = fs.WalkDir(files, "manifests", func(p string, d fs.DirEntry, _ error) error {
		if !d.IsDir() && !seen[p] {
			t.Errorf("embedded %s has no legacy counterpart", p)
		}
		return nil
	})
}

func TestEmbeddedTrustStoreMatchesDocs(t *testing.T) {
	want, err := os.ReadFile(testutil.LegacyPath(t, "..", "docs", "connectors", "trusted-publishers.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := files.ReadFile("trusted-publishers.json")
	if !bytes.Equal(got, want) {
		t.Error("embedded trusted-publishers.json differs from docs/connectors/trusted-publishers.json")
	}
}

func TestPathsLoadBuiltins(t *testing.T) {
	dir, trust, err := Paths()
	if err != nil {
		t.Fatal(err)
	}
	ts := manifest.LoadTrustStore(trust)
	idx, err := manifest.LoadIndex(dir, "optional", ts)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx) != 4 {
		t.Fatalf("builtin index has %d manifests, want 4", len(idx))
	}
}
