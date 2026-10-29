package manifest_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

type canonVec struct {
	Name      string `json:"name"`
	YAML      string `json:"yaml"`
	Canonical string `json:"canonical"`
	SHA256    string `json:"sha256"`
	Error     string `json:"error"`
	Message   string `json:"message"`
}

type verifyResultVec struct {
	Status    string `json:"status"`
	KeyID     any    `json:"key_id"`
	Publisher any    `json:"publisher"`
	Error     string `json:"error"`
}

type verifyVec struct {
	Name       string          `json:"name"`
	Manifest   string          `json:"manifest"`
	Signature  *string         `json:"signature"`
	TrustStore *string         `json:"trust_store"`
	Result     verifyResultVec `json:"result"`
}

type signingFile struct {
	TestKeys  map[string]string `json:"test_keys"`
	Canonical []canonVec        `json:"canonical"`
	Verify    []verifyVec       `json:"verify"`
}

func loadSigning(t *testing.T) signingFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/signing_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f signingFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestCanonicalManifestMatchesPython(t *testing.T) {
	f := loadSigning(t)
	if len(f.Canonical) < 40 {
		t.Fatalf("only %d canonical vectors", len(f.Canonical))
	}
	for _, v := range f.Canonical {
		t.Run(v.Name, func(t *testing.T) {
			got, err := manifest.CanonicalYAML([]byte(v.YAML))
			if v.Error != "" {
				if err == nil {
					t.Fatalf("Python raised %s (%s); Go returned %q", v.Error, v.Message, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != v.Canonical {
				t.Fatalf("canonical bytes differ\n got %q\nwant %q", got, v.Canonical)
			}
			sum := sha256.Sum256(got)
			if hex.EncodeToString(sum[:]) != v.SHA256 {
				t.Fatalf("sha256 differs")
			}
		})
	}
}

// A typed Manifest cannot reproduce the authored YAML (Python hashes the raw
// mapping), but its canonical form must be a fixed point of the YAML path.
func TestCanonicalManifestTypedFixedPoint(t *testing.T) {
	f := loadSigning(t)
	var m manifest.Manifest
	for _, v := range f.Canonical {
		if v.Name == "shipped_style_manifest" {
			// the authored YAML uses the `body` alias: decode through LoadIndex shape
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "m.yaml"), []byte(v.YAML), 0o600); err != nil {
				t.Fatal(err)
			}
			idx, err := manifest.LoadIndex(dir, "optional", manifest.TrustStore{})
			if err != nil || len(idx) != 1 {
				t.Fatalf("LoadIndex: %v %d", err, len(idx))
			}
			for _, e := range idx {
				m = e.Manifest
			}
		}
	}
	if m.Name == "" {
		t.Fatal("shipped_style_manifest vector missing")
	}
	first, err := manifest.CanonicalManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manifest.CanonicalYAML(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("not a fixed point\n%s\n%s", first, second)
	}
}

func writeVerifyCase(t *testing.T, v verifyVec) (string, manifest.TrustStore) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "connector.yaml")
	if err := os.WriteFile(p, []byte(v.Manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if v.Signature != nil {
		if err := os.WriteFile(p+".sig", []byte(*v.Signature), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if v.TrustStore == nil {
		return p, manifest.LoadTrustStore(filepath.Join(dir, "absent.json"))
	}
	tp := filepath.Join(dir, "trust.json")
	if err := os.WriteFile(tp, []byte(*v.TrustStore), 0o600); err != nil {
		t.Fatal(err)
	}
	return p, manifest.LoadTrustStore(tp)
}

// Python error texts that are deterministic (not library messages).
var exactErrors = map[string]bool{
	"manifest digest mismatch":                 true,
	"unsupported connector signature envelope": true,
	"invalid connector trust store":            true,
	"manifest is not a mapping":                true,
}

func TestVerifyAllPythonVectors(t *testing.T) {
	f := loadSigning(t)
	for _, v := range f.Verify {
		t.Run(v.Name, func(t *testing.T) {
			p, trust := writeVerifyCase(t, v)
			res := manifest.VerifyDetail(p, trust)
			if string(res.Status) != v.Result.Status {
				t.Fatalf("status %q, want %q (err %v)", res.Status, v.Result.Status, res.Err)
			}
			if res.KeyID != v.Result.KeyID {
				t.Fatalf("key_id %v, want %v", res.KeyID, v.Result.KeyID)
			}
			wantPub, _ := v.Result.Publisher.(string)
			if res.Publisher != wantPub {
				t.Fatalf("publisher %q, want %q", res.Publisher, wantPub)
			}
			status, err := manifest.Verify(p, trust)
			if status != res.Status {
				t.Fatalf("Verify status %q != VerifyDetail %q", status, res.Status)
			}
			if (status == manifest.SigInvalid) != (err != nil) {
				t.Fatalf("error must be set exactly when invalid: %v", err)
			}
			if exactErrors[v.Result.Error] && err.Error() != v.Result.Error {
				t.Fatalf("error %q, want %q", err, v.Result.Error)
			}
		})
	}
}

func verifyByName(t *testing.T, names ...string) {
	t.Helper()
	f := loadSigning(t)
	want := map[string]string{}
	for _, v := range f.Verify {
		want[v.Name] = v.Result.Status
	}
	for _, n := range names {
		var vec *verifyVec
		for i := range f.Verify {
			if f.Verify[i].Name == n {
				vec = &f.Verify[i]
			}
		}
		if vec == nil {
			t.Fatalf("vector %s missing", n)
		}
		p, trust := writeVerifyCase(t, *vec)
		got, _ := manifest.Verify(p, trust)
		if string(got) != want[n] {
			t.Errorf("%s: %q want %q", n, got, want[n])
		}
	}
}

func TestVerifyTrustedUntrustedInvalid(t *testing.T) {
	verifyByName(t, "trusted", "trusted_second_key", "unsigned", "untrusted_unknown_key_id",
		"untrusted_trust_store_missing", "untrusted_checked_before_digest",
		"wrong_key_claimed_trusted", "bad_algorithm", "trust_store_bad_version")
	// the statuses are the four documented values
	if manifest.SigTrusted != "trusted" || manifest.SigUntrusted != "untrusted" ||
		manifest.SigInvalid != "invalid" || manifest.SigUnsigned != "unsigned" {
		t.Fatal("SigStatus constants")
	}
}

func TestVerifyRejectsTamperedBody(t *testing.T) {
	verifyByName(t, "tampered_manifest_field", "tampered_signature_byte", "tampered_digest",
		"tampered_digest_uppercase", "signature_wrong_length", "signature_bad_base64")
	// formatting-only changes keep a signature valid (canonical form is signed)
	verifyByName(t, "trusted_formatting_only_change", "trusted_key_order_change", "trusted_unicode_manifest")
}

func TestVerifyBase64RejectsNewlines(t *testing.T) {
	f := loadSigning(t)
	var vec verifyVec
	for _, v := range f.Verify {
		if v.Name == "trusted" {
			vec = v
		}
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(*vec.Signature), &env); err != nil {
		t.Fatal(err)
	}
	// Go's base64 decoder drops CR/LF; Python validate=True rejects them.
	env["signature"] = env["signature"].(string)[:4] + "\n" + env["signature"].(string)[4:]
	b, _ := json.Marshal(env)
	s := string(b)
	vec.Signature = &s
	p, trust := writeVerifyCase(t, vec)
	if st, _ := manifest.Verify(p, trust); st != manifest.SigInvalid {
		t.Fatalf("status %q, want invalid", st)
	}
}

func TestCanonicalYAMLBounds(t *testing.T) {
	// alias expansion bomb must fail instead of exhausting memory
	bomb := "a: &a [x, x, x, x, x, x, x, x, x, x]\n"
	prev := "a"
	for _, n := range []string{"b", "c", "d", "e", "f", "g", "h"} {
		bomb += n + ": &" + n + " [*" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + "]\n"
		prev = n
	}
	if _, err := manifest.CanonicalYAML([]byte(bomb)); err == nil {
		t.Fatal("alias bomb accepted")
	}
	if _, err := manifest.CanonicalYAML([]byte("a: &a [*a]\n")); err == nil {
		t.Fatal("recursive anchor accepted")
	}
}

func TestCanonicalYAMLMergeSequenceFirstWins(t *testing.T) {
	got, err := manifest.CanonicalYAML([]byte("a: &a {x: 1, y: 1}\nb: &b {x: 2, z: 2}\nc:\n  <<: [*a, *b]\n  w: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":{"x":1,"y":1},"b":{"x":2,"z":2},"c":{"w":0,"x":1,"y":1,"z":2}}`
	if string(got) != want {
		t.Fatalf("got %s want %s (PyYAML output)", got, want)
	}
}

func TestCanonicalYAMLNumericKeyOrder(t *testing.T) {
	got, err := manifest.CanonicalYAML([]byte("10: a\n2: b\n1.5: c\ntrue: d\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"true":"d","1.5":"c","2":"b","10":"a"}`; string(got) != want {
		t.Fatalf("got %s want %s (PyYAML output)", got, want)
	}
}
