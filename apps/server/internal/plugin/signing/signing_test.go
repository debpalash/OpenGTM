package signing

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPolicies(t *testing.T) {
	for in, want := range map[string]string{"": "optional", "optional": "optional", "required": "required", "sometimes": "required", "OPTIONAL": "required"} {
		if got := NormalizePolicy(in); got != want {
			t.Errorf("NormalizePolicy(%q) = %s", in, got)
		}
	}
	t.Setenv("CONNECTOR_SIGNATURE_POLICY", "  Required ")
	if PolicyFromEnv() != "required" {
		t.Fatal("env policy is stripped and lower-cased")
	}
	t.Setenv("CONNECTOR_SIGNATURE_POLICY", "off")
	if PolicyFromEnv() != "required" {
		t.Fatal("unknown env policy fails closed")
	}
	if !Rejects("optional", Result{Status: StatusUntrusted}) || Rejects("optional", Result{Status: StatusUnsigned}) || !Rejects("required", Result{Status: StatusUnsigned}) {
		t.Fatal("Rejects")
	}
}

func TestEnvelopeFormatAndVerify(t *testing.T) {
	seed := sha256.Sum256([]byte("signing-unit-test"))
	key := ed25519.NewKeyFromSeed(seed[:])
	dir := t.TempDir()
	m := filepath.Join(dir, "m.yaml")
	os.WriteFile(m, []byte("name: x\nlist: [1, 2.0]\n"), 0o644)
	sig, err := SignManifestWithKey(m, key, "k1")
	if err != nil {
		t.Fatal(err)
	}
	env, _ := os.ReadFile(sig)
	payload := `{"list":[1,2.0],"name":"x"}`
	sum := sha256.Sum256([]byte(payload))
	if !strings.HasPrefix(string(env), "{\n  \"algorithm\": \"Ed25519\",\n  \"key_id\": \"k1\",\n  \"manifest_sha256\": \""+hex.EncodeToString(sum[:])+"\",\n") || !strings.HasSuffix(string(env), "\"signature_version\": \"1\"\n}\n") {
		t.Fatalf("envelope format:\n%s", env)
	}
	store := filepath.Join(dir, "trust.json")
	b, _ := json.Marshal(map[string]any{"version": 1, "keys": []any{TrustEntry("k1", "Pub", key.Public().(ed25519.PublicKey))}})
	os.WriteFile(store, b, 0o644)
	if r := VerifyManifest(m, store); r.Status != StatusTrusted || r.Publisher != "Pub" || r.KeyID != "k1" {
		t.Fatalf("%+v", r)
	}
	if r := VerifyManifest(m, filepath.Join(dir, "missing.json")); r.Status != StatusUntrusted {
		t.Fatalf("missing store: %+v", r)
	}
	if r := VerifyManifest(filepath.Join(dir, "nosig.yaml"), store); r.Status != StatusUnsigned {
		t.Fatalf("unsigned: %+v", r)
	}
	if _, err := b64Strict("YQ\n=="); err == nil {
		t.Fatal("newlines are not base64 data under validate=True")
	}
	if b, err := b64Strict("YR=="); err != nil || string(b) != "a" {
		t.Fatal("non-zero trailing bits are accepted like Python")
	}
	pemBytes, _ := EncodePrivateKey(key)
	kp := filepath.Join(dir, "k.pem")
	os.WriteFile(kp, pemBytes, 0o600)
	if loaded, err := LoadPrivateKey(kp); err != nil || !loaded.Equal(key) {
		t.Fatalf("pem round trip: %v", err)
	}
}
