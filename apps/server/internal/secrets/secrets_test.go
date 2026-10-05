package secrets

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(kv map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := kv[k]; return v, ok }
}

func TestFernetKnownAnswerFromTheSpec(t *testing.T) {
	// Test vector from github.com/fernet/spec (generate.json).
	key, err := ParseKey("cw_0x689RpI-jtRR7oE8h_eQsKImvJapLeSbXpwF4e4=")
	if err != nil {
		t.Fatal(err)
	}
	token := "gAAAAAAdwJ6wAAECAwQFBgcICQoLDA0ODy021cpGVWKZ_eEwCGM4BLLF_5CV9dOPmrhuVUPgJobwOz7JcbmrR64jVmpU4IwqDA=="
	pt, err := key.Decrypt(token)
	if err != nil || string(pt) != "hello" {
		t.Fatalf("spec vector: %q %v", pt, err)
	}
}

func TestRoundTripAndTamper(t *testing.T) {
	c := NewCipher(DeriveKey("unit-test"))
	for _, v := range []string{"x", "sk_live_0123456789abcdef", strings.Repeat("a", 16), strings.Repeat("é€", 500), "multi\nline"} {
		enc, err := c.Encrypt(v)
		if err != nil || !strings.HasPrefix(enc, EnvelopePrefix) || (len(v) > 8 && strings.Contains(enc, v)) {
			t.Fatalf("encrypt %q: %q %v", v, enc, err)
		}
		other, _ := c.Encrypt(v)
		if enc == other {
			t.Fatal("encryption must be randomized (fresh IV)")
		}
		got, err := c.Decrypt(enc)
		if err != nil || got != v {
			t.Fatalf("round trip %q: %q %v", v, got, err)
		}
	}
	enc, _ := c.Encrypt("secret-value")
	raw, _ := base64.URLEncoding.DecodeString(strings.TrimPrefix(enc, EnvelopePrefix))
	for i := range raw {
		bad := append([]byte(nil), raw...)
		bad[i] ^= 0x01
		if _, err := c.Decrypt(EnvelopePrefix + base64.URLEncoding.EncodeToString(bad)); err == nil {
			t.Fatalf("flipping byte %d went unnoticed", i)
		}
	}
	for _, bad := range []string{"", EnvelopePrefix, EnvelopePrefix + "AAAA", "plaintext", "enc:v2:vault:vault:v1:abc", enc[:len(enc)-4]} {
		if _, err := c.Decrypt(bad); err == nil {
			t.Fatalf("Decrypt(%q) accepted", bad)
		}
	}
	if _, err := NewCipher(DeriveKey("another")).Decrypt(enc); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err := c.Decrypt("enc:v2:vault:abc"); !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("vault envelope: %v", err)
	}
	// Errors never contain the plaintext.
	if _, err := NewCipher(DeriveKey("another")).Decrypt(enc); err != nil && strings.Contains(err.Error(), "secret-value") {
		t.Fatal("error leaks plaintext")
	}
}

func TestFromEnvMirrorsPython(t *testing.T) {
	good := "cw_0x689RpI-jtRR7oE8h_eQsKImvJapLeSbXpwF4e4="
	gk, _ := ParseKey(good)
	enc := func(c *Cipher) string { s, _ := c.Encrypt("v"); return s }
	decrypts := func(c *Cipher, s string) bool { v, err := c.Decrypt(s); return err == nil && v == "v" }

	c, err := FromEnv(env(map[string]string{"SECRETS_MASTER_KEY": "  " + good + " ", "APP_ENV": "production"}))
	if err != nil || !decrypts(c, enc(NewCipher(gk))) {
		t.Fatalf("a valid Fernet master key is used verbatim: %v", err)
	}
	c, err = FromEnv(env(map[string]string{"SECRETS_MASTER_KEY": "just a passphrase", "APP_ENV": "production"}))
	if err != nil || !decrypts(c, enc(NewCipher(DeriveKey("just a passphrase")))) {
		t.Fatalf("other master key material is hashed: %v", err)
	}
	c, err = FromEnv(env(map[string]string{"SECRET_KEY": "a-strong-key", "APP_ENV": "production"}))
	if err != nil || !decrypts(c, enc(NewCipher(DeriveKey("a-strong-key")))) {
		t.Fatalf("SECRET_KEY fallback: %v", err)
	}
	// Development environments tolerate the insecure default, production does not.
	if c, err = FromEnv(env(nil)); err != nil || !decrypts(c, enc(NewCipher(DeriveKey(insecureDefaultSecretKey)))) {
		t.Fatalf("dev default: %v", err)
	}
	for _, appEnv := range []string{"production", "staging", ""} {
		_, err := FromEnv(env(map[string]string{"APP_ENV": appEnv}))
		if !errors.Is(err, ErrNoKey) { // an empty APP_ENV is not a development one either
			t.Fatalf("APP_ENV=%q with the insecure key must fail closed, got %v", appEnv, err)
		}
	}
	if _, err := FromEnv(env(map[string]string{"SECRETS_PROVIDER": "vault_transit", "SECRETS_MASTER_KEY": good})); !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("vault provider: %v", err)
	}
	if _, err := FromEnv(env(map[string]string{"SECRETS_PROVIDER": " LOCAL ", "SECRETS_MASTER_KEY": good})); err != nil {
		t.Fatalf("provider is case-insensitive: %v", err)
	}
}

// ---- cross-language -----------------------------------------------------------

const repoRoot = "../../../.."

// pyScript encrypts every case with the Python helpers and decrypts what Go
// produced, under the same environment, and reports the results as JSON.
const pyScript = `
import json, sys
from apps.api.services.workspace.secrets import encrypt_value, decrypt_value
req = json.load(sys.stdin)
out = {"encrypted": [encrypt_value(v) for v in req["plain"]], "decrypted": []}
for tok in req["tokens"]:
    try:
        out["decrypted"].append(decrypt_value(tok))
    except Exception as exc:
        out["decrypted"].append("ERROR:" + type(exc).__name__)
print(json.dumps(out))
`

func runPython(t *testing.T, envv map[string]string, plain, tokens []string) (enc, dec []string) {
	t.Helper()
	if os.Getenv("OPENGTM_SKIP_PYTHON") == "1" {
		t.Skip("OPENGTM_SKIP_PYTHON=1")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not installed; cross-language test needs the Python app")
	}
	root, _ := filepath.Abs(repoRoot)
	if _, err := os.Stat(filepath.Join(root, "apps", "api", "services", "workspace", "secrets.py")); err != nil {
		t.Skip("Python sources not present")
	}
	cmd := exec.Command("uv", "run", "--quiet", "python", "-c", pyScript)
	cmd.Dir = root
	for _, kv := range os.Environ() {
		switch strings.SplitN(kv, "=", 2)[0] {
		case "SECRETS_PROVIDER", "SECRETS_MASTER_KEY", "SECRET_KEY", "APP_ENV":
		default:
			cmd.Env = append(cmd.Env, kv)
		}
	}
	for k, v := range envv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if plain == nil {
		plain = []string{}
	}
	if tokens == nil {
		tokens = []string{}
	}
	in, _ := json.Marshal(map[string]any{"plain": plain, "tokens": tokens})
	cmd.Stdin = bytes.NewReader(in)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python: %v\n%s\n%s", err, out, stderr.String())
	}
	var res struct{ Encrypted, Decrypted []string }
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("python output %q: %v", out, err)
	}
	return res.Encrypted, res.Decrypted
}

func TestCrossLanguageEnvelopeAndKeyDerivation(t *testing.T) {
	values := []string{"sk_live_abc123", "ünïcode ✓ key", strings.Repeat("z", 1000), "x"}
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"fernet master key", map[string]string{"SECRETS_MASTER_KEY": "cw_0x689RpI-jtRR7oE8h_eQsKImvJapLeSbXpwF4e4=", "APP_ENV": "production", "SECRET_KEY": "strong-secret-key"}},
		{"master key with surrounding space", map[string]string{"SECRETS_MASTER_KEY": "  cw_0x689RpI-jtRR7oE8h_eQsKImvJapLeSbXpwF4e4=\n", "APP_ENV": "production", "SECRET_KEY": "strong-secret-key"}},
		{"passphrase", map[string]string{"SECRETS_MASTER_KEY": "correct horse battery staple", "APP_ENV": "production", "SECRET_KEY": "strong-secret-key"}},
		{"key with a stray character", map[string]string{"SECRETS_MASTER_KEY": "cw_0x689RpI-jtRR7oE8h_eQsKImvJapLeSbXpwF4e4=!", "APP_ENV": "production", "SECRET_KEY": "strong-secret-key"}},
		{"unpadded key is material", map[string]string{"SECRETS_MASTER_KEY": "cw_0x689RpI-jtRR7oE8h_eQsKImvJapLeSbXpwF4e4", "APP_ENV": "production", "SECRET_KEY": "strong-secret-key"}},
		{"31 byte key is material", map[string]string{"SECRETS_MASTER_KEY": "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNk", "APP_ENV": "production", "SECRET_KEY": "strong-secret-key"}},
		{"SECRET_KEY", map[string]string{"SECRET_KEY": "a-strong-production-secret", "APP_ENV": "production"}},
		{"dev default", map[string]string{"APP_ENV": "dev", "SECRET_KEY": insecureDefaultSecretKey}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := FromEnv(func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok })
			if err != nil {
				t.Fatal(err)
			}
			var goTokens []string
			for _, v := range values {
				tok, err := c.Encrypt(v)
				if err != nil {
					t.Fatal(err)
				}
				goTokens = append(goTokens, tok)
			}
			pyEnc, pyDec := runPython(t, tc.env, values, goTokens)
			for i, v := range values {
				if pyDec[i] != v {
					t.Errorf("Python could not read Go's token for %q: %q", v, pyDec[i])
				}
				if got, err := c.Decrypt(pyEnc[i]); err != nil || got != v {
					t.Errorf("Go could not read Python's token for %q: %q %v", v, got, err)
				}
			}
		})
	}
	// A different key on the other side fails on both.
	c, _ := FromEnv(env(map[string]string{"SECRET_KEY": "key-one", "APP_ENV": "production"}))
	tok, _ := c.Encrypt("v")
	_, dec := runPython(t, map[string]string{"SECRET_KEY": "key-two", "APP_ENV": "production"}, nil, []string{tok})
	if !strings.HasPrefix(dec[0], "ERROR:") {
		t.Fatalf("Python decrypted with the wrong key: %q", dec[0])
	}
}

func TestEncryptStampsCurrentTime(t *testing.T) {
	c := NewCipher(DeriveKey("t"))
	c.now = func() time.Time { return time.Unix(1_000_000_000, 0) }
	enc, _ := c.Encrypt("v")
	raw, _ := base64.URLEncoding.DecodeString(strings.TrimPrefix(enc, EnvelopePrefix))
	if raw[0] != 0x80 || raw[5] != 0x3b || raw[6] != 0x9a || raw[7] != 0xca || raw[8] != 0x00 { // 1e9 = 0x3B9ACA00
		t.Fatalf("header %x", raw[:9])
	}
}
