package manifest_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

type connectorVec struct {
	Path   string `json:"path"`
	SHA256 string `json:"manifest_sha256"`
	ID     string `json:"id"`
	Sig    struct {
		Status string `json:"status"`
	} `json:"signature"`
	Capabilities []string `json:"capabilities"`
	Credential   *string  `json:"credential_key"`
	Cost         float64  `json:"cost_per_lookup"`
}

type reportVec struct {
	OK         bool           `json:"ok"`
	Connectors []connectorVec `json:"connectors"`
	Errors     []struct {
		Error string `json:"error"`
	} `json:"errors"`
}

type registryFile struct {
	CatalogOptional reportVec `json:"catalog_optional"`
	Shipped         []struct {
		Path string `json:"path"`
		Text string `json:"text"`
	} `json:"shipped"`
	Loaded []struct {
		Name         string   `json:"name"`
		Capability   string   `json:"capability"`
		AllCaps      []string `json:"all_capabilities"`
		CatalogEntry struct {
			Credential *string `json:"credential_key"`
		} `json:"catalog_entry"`
	} `json:"loaded"`
	DirectoryRules []struct {
		Name   string            `json:"name"`
		Files  map[string]string `json:"files"`
		Policy string            `json:"policy"`
		Report reportVec         `json:"report"`
	} `json:"directory_rules"`
}

func loadRegistry(t *testing.T) registryFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/registry_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f registryFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func keyATrust(t *testing.T, dir string) manifest.TrustStore {
	t.Helper()
	f := loadSigning(t)
	body, _ := json.Marshal(map[string]any{"version": 1, "keys": []any{
		map[string]any{"key_id": "key-a", "public_key": f.TestKeys["key-a"], "publisher": "Contract Publisher"},
	}})
	p := filepath.Join(dir, "trust-store.json")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return manifest.LoadTrustStore(p)
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadIndexHashEqualsPython(t *testing.T) {
	reg := loadRegistry(t)
	dir := t.TempDir()
	files := map[string]string{}
	for _, s := range reg.Shipped {
		files[s.Path] = s.Text
	}
	writeTree(t, dir, files)
	idx, err := manifest.LoadIndex(dir, "optional", manifest.TrustStore{})
	if err != nil {
		t.Fatal(err)
	}
	if len(idx) != len(reg.Loaded) || len(idx) != 4 {
		t.Fatalf("index has %d entries, Python loaded %d", len(idx), len(reg.Loaded))
	}
	bySHA := map[string]string{}
	for _, c := range reg.CatalogOptional.Connectors {
		bySHA[c.ID] = c.SHA256
	}
	for _, l := range reg.Loaded {
		e, ok := idx[l.Name]
		if !ok {
			t.Fatalf("%s missing from index", l.Name)
		}
		if e.SHA256 != bySHA[l.Name] || e.SHA256 == "" {
			t.Errorf("%s sha %s, Python %s", l.Name, e.SHA256, bySHA[l.Name])
		}
		if e.Manifest.Capability != l.Capability {
			t.Errorf("%s capability %q want %q", l.Name, e.Manifest.Capability, l.Capability)
		}
		if l.CatalogEntry.Credential != nil && e.Manifest.Auth.EnvVar != *l.CatalogEntry.Credential {
			t.Errorf("%s env_var %q want %q", l.Name, e.Manifest.Auth.EnvVar, *l.CatalogEntry.Credential)
		}
	}
	// every directory_rules case Python reports ok must index with the same hash
	for _, c := range reg.DirectoryRules {
		if !c.Report.OK || len(c.Report.Connectors) == 0 {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			d := t.TempDir()
			writeTree(t, d, c.Files)
			got, err := manifest.LoadIndex(d, c.Policy, keyATrust(t, d))
			if err != nil {
				t.Fatal(err)
			}
			for _, py := range c.Report.Connectors {
				e, ok := got[py.ID]
				if !ok {
					t.Fatalf("%s not indexed", py.ID)
				}
				if e.SHA256 != py.SHA256 {
					t.Fatalf("sha %s want %s", e.SHA256, py.SHA256)
				}
			}
		})
	}
}

func TestLoadIndexPolicyRequired(t *testing.T) {
	reg := loadRegistry(t)
	pick := func(name string) map[string]string {
		for _, c := range reg.DirectoryRules {
			if c.Name == name {
				return c.Files
			}
		}
		t.Fatalf("case %s missing", name)
		return nil
	}
	cases := []struct {
		name, policy, files string
		want                int
	}{
		{"signed trusted, required", "required", "valid_signed_required", 1},
		{"unsigned, required", "required", "valid_unsigned_required_rejected", 0},
		{"unsigned, optional", "optional", "valid_unsigned_optional", 1},
		{"unsigned, unknown policy means required", "bogus", "valid_unsigned_optional", 0},
		{"unsigned, empty policy means required", "", "valid_unsigned_optional", 0},
		{"unsigned, padded mixed-case optional", "  OPTIONAL ", "valid_unsigned_optional", 1},
		{"signed trusted, optional", "optional", "valid_signed_required", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := t.TempDir()
			writeTree(t, d, pick(c.files))
			idx, err := manifest.LoadIndex(d, c.policy, keyATrust(t, d))
			if err != nil {
				t.Fatal(err)
			}
			if len(idx) != c.want {
				t.Fatalf("%d entries, want %d", len(idx), c.want)
			}
		})
	}
}

func TestLoadIndexSkipsInvalid(t *testing.T) {
	reg := loadRegistry(t)
	good := ""
	files := map[string]string{}
	for _, c := range reg.DirectoryRules {
		files[c.Name] = ""
		if c.Name == "valid_unsigned_optional" {
			good = c.Files["email/c.yaml"]
		}
	}
	skipped := []string{"signed_untrusted_rejected", "signed_tampered_rejected", "unknown_manifest_version", "missing_required_fields"}
	d := t.TempDir()
	for _, c := range reg.DirectoryRules {
		for _, n := range skipped {
			if c.Name != n {
				continue
			}
			for rel, content := range c.Files {
				writeTree(t, d, map[string]string{n + "/" + rel: content})
			}
		}
	}
	writeTree(t, d, map[string]string{
		"z/good.yaml":        good,
		"broken_yaml.yaml":   "a: {",
		"not_mapping.yml":    "- a\n- b\n",
		"readme.txt":         "name: ignored\n",
		"also.yaml.sig.bak":  "{}",
		"deeper/x/leaf.yaml": "name: only_name\n",
	})
	idx, err := manifest.LoadIndex(d, "optional", keyATrust(t, d))
	if err != nil {
		t.Fatal(err)
	}
	if len(idx) != 1 {
		names := []string{}
		for k := range idx {
			names = append(names, k)
		}
		t.Fatalf("index %v, want only contract_email", names)
	}
	if _, ok := idx["contract_email"]; !ok {
		t.Fatal("good manifest dropped because of bad siblings")
	}
}

func TestLoadIndexMissingDirIsEmpty(t *testing.T) {
	idx, err := manifest.LoadIndex(filepath.Join(t.TempDir(), "nope"), "optional", manifest.TrustStore{})
	if err != nil || len(idx) != 0 {
		t.Fatalf("%v %v", idx, err)
	}
}

func TestLoadIndexLaterPathWins(t *testing.T) {
	reg := loadRegistry(t)
	var base string
	for _, c := range reg.DirectoryRules {
		if c.Name == "valid_unsigned_optional" {
			base = c.Files["email/c.yaml"]
		}
	}
	d := t.TempDir()
	// Python sorts Path objects component-wise: "a/b.yaml" < "a.yaml" (string order says the opposite).
	writeTree(t, d, map[string]string{
		"a.yaml":   base + "description: from a.yaml\n",
		"a/b.yaml": base + "description: from a/b.yaml\n",
	})
	// base already has description; override by dropping it so the appended key is unique
	writeTree(t, d, map[string]string{
		"a.yaml":   stripLine(base, "description:") + "description: from a.yaml\n",
		"a/b.yaml": stripLine(base, "description:") + "description: from a/b.yaml\n",
	})
	idx, err := manifest.LoadIndex(d, "optional", manifest.TrustStore{})
	if err != nil {
		t.Fatal(err)
	}
	if got := idx["contract_email"].Manifest.Description; got != "from a.yaml" {
		t.Fatalf("winner %q, want the later Python path a.yaml", got)
	}
}

func stripLine(text, prefix string) string {
	out := ""
	for _, l := range splitLines(text) {
		if len(l) >= len(prefix) && l[:len(prefix)] == prefix {
			continue
		}
		out += l + "\n"
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func TestManifestPathTraversalRejected(t *testing.T) {
	reg := loadRegistry(t)
	var good string
	for _, c := range reg.DirectoryRules {
		if c.Name == "valid_unsigned_optional" {
			good = c.Files["email/c.yaml"]
		}
	}
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	inside := filepath.Join(root, "inside")
	writeTree(t, outside, map[string]string{"evil.yaml": good, "sub/evil2.yaml": good})
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	// symlinked directory and symlinked file pointing out of the tree
	if err := os.Symlink(outside, filepath.Join(inside, "linkdir")); err != nil {
		t.Skip("symlinks unavailable: ", err)
	}
	if err := os.Symlink(filepath.Join(outside, "evil.yaml"), filepath.Join(inside, "linkfile.yaml")); err != nil {
		t.Fatal(err)
	}
	idx, err := manifest.LoadIndex(inside, "optional", manifest.TrustStore{})
	if err != nil {
		t.Fatal(err)
	}
	if len(idx) != 0 {
		t.Fatalf("symlinks were followed: %d entries", len(idx))
	}
	// a real file with a symlinked signature is not trusted either
	writeTree(t, inside, map[string]string{"real.yaml": good})
	if err := os.Symlink(filepath.Join(outside, "evil.yaml"), filepath.Join(inside, "real.yaml.sig")); err != nil {
		t.Fatal(err)
	}
	if idx, _ = manifest.LoadIndex(inside, "optional", manifest.TrustStore{}); len(idx) != 0 {
		t.Fatal("symlinked signature accepted")
	}
	// '..' in the directory argument is refused outright
	if _, err := manifest.LoadIndex(inside+"/../outside", "optional", manifest.TrustStore{}); err != manifest.ErrPathTraversal {
		t.Fatalf("err %v, want ErrPathTraversal", err)
	}
}
