package connectors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

type vecCase struct {
	Name           string            `json:"name"`
	Files          map[string]string `json:"files"`
	Policy         string            `json:"policy"`
	TrustStore     *string           `json:"trust_store"`
	Report         json.RawMessage   `json:"report"`
	Loaded         []string          `json:"loaded"`
	Compatibility  json.RawMessage   `json:"compatibility"`
	DirectoryExist bool              `json:"directory_exists"`
	Portable       []bool            `json:"error_is_portable"`
}

type vecFile struct {
	Cases     []vecCase `json:"cases"`
	PolicyEnv []struct {
		Env    *string `json:"env"`
		Policy string  `json:"policy"`
	} `json:"signature_policy_env"`
}

func loadVec(t *testing.T) vecFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/connector_report.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vecFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// materialise writes a vector case under a temp root and returns (root, dir, trust).
func materialise(t *testing.T, c vecCase) (string, string, manifest.TrustStore) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "m")
	if _, nodir := c.Files["__nodir__"]; !nodir {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for rel, body := range c.Files {
		if rel == "__nodir__" {
			continue
		}
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	trustPath := filepath.Join(root, "trust.json")
	if c.TrustStore != nil {
		if err := os.WriteFile(trustPath, []byte(*c.TrustStore), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, dir, manifest.LoadTrustStore(trustPath)
}

func asAny(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// relativise rewrites error paths from the temp root back to the vector layout.
func relativise(r Report, root string) Report {
	out := r
	out.Errors = append([]ReportError{}, r.Errors...)
	for i := range out.Errors {
		out.Errors[i].Path = strings.TrimPrefix(out.Errors[i].Path, root+string(filepath.Separator))
	}
	return out
}

// sameReport compares two report documents; error text is compared only where
// the vector marks it portable (library messages differ between runtimes).
func sameReport(t *testing.T, name string, got, want any, portable []bool) {
	t.Helper()
	g, w := got.(map[string]any), want.(map[string]any)
	ge, we := g["errors"].([]any), w["errors"].([]any)
	if len(ge) != len(we) {
		t.Errorf("%s: %d errors, want %d: %v", name, len(ge), len(we), ge)
		return
	}
	for i := range we {
		gm, wm := ge[i].(map[string]any), we[i].(map[string]any)
		if gm["path"] != wm["path"] {
			t.Errorf("%s: error %d path %v, want %v", name, i, gm["path"], wm["path"])
		}
		if i < len(portable) && portable[i] && gm["error"] != wm["error"] {
			t.Errorf("%s: error %d = %q, want %q", name, i, gm["error"], wm["error"])
		}
		if gm["error"] == "" {
			t.Errorf("%s: error %d is empty", name, i)
		}
	}
	g2, w2 := map[string]any{}, map[string]any{}
	for k, v := range g {
		if k != "errors" {
			g2[k] = v
		}
	}
	for k, v := range w {
		if k != "errors" {
			w2[k] = v
		}
	}
	if !reflect.DeepEqual(g2, w2) {
		gb, _ := json.Marshal(g2)
		wb, _ := json.Marshal(w2)
		t.Errorf("%s: report differs\n got: %s\nwant: %s", name, gb, wb)
	}
}

func TestValidateDirectoryVectors(t *testing.T) {
	vec := loadVec(t)
	if len(vec.Cases) < 70 {
		t.Fatalf("vector file shrank: %d cases", len(vec.Cases))
	}
	for _, c := range vec.Cases {
		t.Run(c.Name, func(t *testing.T) {
			root, dir, trust := materialise(t, c)
			rep := relativise(ValidateDirectory(dir, c.Policy, trust), root)
			b, err := json.Marshal(rep)
			if err != nil {
				t.Fatal(err)
			}
			sameReport(t, c.Name, asAny(t, b), asAny(t, c.Report), c.Portable)
			cb, _ := json.Marshal(relativise(Compatibility(ValidateDirectory(dir, c.Policy, trust)), root))
			sameReport(t, c.Name+"/compat", asAny(t, cb), asAny(t, c.Compatibility), c.Portable)
		})
	}
}

func TestReportKeyOrder(t *testing.T) {
	vec := loadVec(t)
	var c vecCase
	for _, x := range vec.Cases {
		if x.Name == "unsigned_and_signed_policy_required" {
			c = x
		}
	}
	_, dir, trust := materialise(t, c)
	b, _ := json.Marshal(ValidateDirectory(dir, c.Policy, trust))
	order := []string{`"ok"`, `"manifest_version"`, `"signature_policy"`, `"count"`, `"connectors"`, `"errors"`,
		`"path"`, `"manifest_sha256"`, `"signature"`, `"status"`, `"key_id"`, `"publisher"`, `"id"`, `"name"`, `"author"`,
		`"homepage"`, `"license"`, `"description"`, `"capabilities"`, `"tags"`, `"cost_per_lookup"`, `"default_confidence"`, `"credential_key"`}
	pos := -1
	for _, k := range order {
		i := strings.Index(string(b), k)
		if i < 0 || i < pos {
			// "manifest_version" also appears inside entries; only the first occurrence matters
			t.Fatalf("key %s out of order (pos %d, prev %d) in %s", k, i, pos, b)
		}
		pos = i
	}
}

func TestEffectivePolicy(t *testing.T) {
	for _, row := range loadVec(t).PolicyEnv {
		raw, set := "", false
		if row.Env != nil {
			raw, set = *row.Env, true
		}
		if got := EffectivePolicy(raw, set); got != row.Policy {
			t.Errorf("EffectivePolicy(%q,%v) = %q, want %q", raw, set, got, row.Policy)
		}
	}
}

func caseByName(t *testing.T, name string) vecCase {
	t.Helper()
	for _, c := range loadVec(t).Cases {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no vector %s", name)
	return vecCase{}
}

func TestPolicyRequiredRejectsUnsigned(t *testing.T) {
	c := caseByName(t, "unsigned_and_signed_policy_required")
	_, dir, trust := materialise(t, c)
	rep := ValidateDirectory(dir, "required", trust)
	if rep.OK || rep.Count != 1 || rep.Connectors[0].ID != "vec_good" || len(rep.Errors) != 1 ||
		rep.Errors[0].Error != "connector signature is unsigned:" {
		t.Fatalf("%+v", rep)
	}
	opt := ValidateDirectory(dir, "optional", trust)
	if !opt.OK || opt.Count != 2 {
		t.Fatalf("optional: %+v", opt)
	}
	if weird := ValidateDirectory(dir, "whatever", trust); weird.SignaturePolicy != "required" || weird.OK {
		t.Fatalf("unknown policy must become required: %+v", weird)
	}
}

func TestCredentialKeyNeverReturned(t *testing.T) {
	c := caseByName(t, "unsigned_and_signed_policy_required")
	_, dir, trust := materialise(t, c)
	rep := ValidateDirectory(dir, "optional", trust)
	if b, _ := json.Marshal(rep); !strings.Contains(string(b), "credential_key") {
		t.Fatal("the raw report keeps credential_key (Python validate_manifest_directory)")
	}
	b, _ := json.Marshal(Compatibility(rep))
	if strings.Contains(string(b), "credential_key") || strings.Contains(string(b), "CONTRACT_TRUSTED_KEY") {
		t.Fatalf("compatibility leaked the credential key: %s", b)
	}
	if rb, _ := json.Marshal(rep); !strings.Contains(string(rb), "CONTRACT_TRUSTED_KEY") {
		t.Fatal("Compatibility must not mutate its input")
	}
}

func TestSymlinkOutsideRootRejected(t *testing.T) {
	c := caseByName(t, "unsigned_and_signed_policy_required")
	root, dir, trust := materialise(t, c)
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "email", "vec_unsigned.yaml"))
	if err := os.WriteFile(filepath.Join(outside, "evil.yaml"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "evil.yaml"), filepath.Join(dir, "email", "link.yaml")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linkdir")); err != nil {
		t.Fatal(err)
	}
	rep := ValidateDirectory(dir, "optional", trust)
	if rep.OK {
		t.Fatal("symlinks must be reported as errors")
	}
	for _, c := range rep.Connectors {
		if c.ID == "evil" || strings.Contains(c.Path, "link") {
			t.Fatalf("symlink target was loaded: %+v", c)
		}
	}
	var linkErrs int
	for _, e := range rep.Errors {
		if strings.HasSuffix(e.Path, "link.yaml") || strings.HasSuffix(e.Path, "linkdir") {
			linkErrs++
			if !strings.Contains(e.Error, "symlink") {
				t.Errorf("error %q should name the symlink", e.Error)
			}
		}
	}
	if linkErrs != 2 {
		t.Fatalf("want 2 symlink errors, got %+v", rep.Errors)
	}
	// the catalog loader skips them too
	scan := scanDirectory(dir, "optional", trust)
	for _, l := range scan.loaded {
		if l.Entry.Manifest.Name == "vec_unsigned" && strings.Contains(l.Path, "link") {
			t.Fatalf("catalog loaded through a symlink")
		}
	}
}

func TestDotDotDirectoryRejected(t *testing.T) {
	root := t.TempDir()
	rep := ValidateDirectory(filepath.Join(root, "a", "..", "b"), "optional", manifest.TrustStore{})
	if rep.OK || len(rep.Errors) != 1 || rep.Count != 0 {
		t.Fatalf("%+v", rep)
	}
}

func TestOversizedManifestIsAnError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "big.yaml"), []byte(strings.Repeat("a: 1\n", 300000)), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := ValidateDirectory(root, "optional", manifest.TrustStore{})
	if rep.OK || len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0].Error, "1 MiB") {
		t.Fatalf("%+v", rep)
	}
}
