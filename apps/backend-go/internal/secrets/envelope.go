package secrets

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// EncPrefix marks a local Fernet envelope: enc:v1:<fernet token>.
	EncPrefix = "enc:v1:"
	// VaultPrefix marks a Vault Transit envelope: enc:v2:vault:<vault ciphertext>.
	VaultPrefix = "enc:v2:vault:"
	// InsecureSecretKey is the shipped development SECRET_KEY default.
	InsecureSecretKey = "INSECURE_FALLBACK_KEY_FOR_DEVELOPMENT_ONLY"
)

// Config carries the settings that drive the envelope (SECRETS_PROVIDER,
// SECRETS_MASTER_KEY, SECRET_KEY, APP_ENV, VAULT_*).
type Config struct {
	Provider, MasterKey, SecretKey, AppEnv                                              string
	VaultAddr, VaultToken, VaultTokenFile, VaultTransitKey, VaultNamespace, VaultCACert string
}

// ErrWrongKey is returned for every failure to decrypt a local envelope.
var ErrWrongKey = errors.New("failed to decrypt a per-workspace secret: the master key does not match the key used to encrypt it (rotated/incorrect SECRETS_MASTER_KEY?)")

// Envelope encrypts and decrypts at-rest secret values (secrets.py:123-175).
type Envelope struct {
	provider string
	fernet   Fernet
	keyErr   error // deferred _load_master_key failure; surfaces on use
	vault    *vaultClient
}

// NewEnvelope validates the provider. A master-key refusal (insecure default
// outside dev) is deferred to Encrypt/Decrypt of enc:v1 values, as in Python,
// so plaintext passthrough keeps working.
func NewEnvelope(cfg Config, hc *http.Client) (*Envelope, error) {
	provider := strings.ToLower(pyStrip(cfg.Provider))
	if provider == "" {
		provider = "local"
	}
	if provider != "local" && provider != "vault_transit" {
		return nil, fmt.Errorf("unsupported SECRETS_PROVIDER: %s", provider)
	}
	e := &Envelope{provider: provider, vault: newVaultClient(cfg, hc)}
	e.fernet, e.keyErr = loadMasterKey(cfg)
	return e, nil
}

var devEnvs = map[string]bool{"dev": true, "development": true, "test": true, "testing": true, "local": true}

func loadMasterKey(cfg Config) (Fernet, error) {
	if raw := pyStrip(cfg.MasterKey); raw != "" {
		return FromMaster(raw), nil
	}
	if strings.Contains(cfg.SecretKey, InsecureSecretKey) && !devEnvs[strings.ToLower(pyStrip(cfg.AppEnv))] {
		return Fernet{}, fmt.Errorf("Refusing to encrypt per-workspace secrets: SECRETS_MASTER_KEY is "+
			"unset and SECRET_KEY is the insecure default while APP_ENV='%s'. Set SECRETS_MASTER_KEY "+
			"(a Fernet key) or a strong SECRET_KEY in the environment/.env.", cfg.AppEnv)
	}
	return DeriveKey(cfg.SecretKey), nil
}

// pyStrip mimics Python str.strip() (adds the \x1c-\x1f separators Go omits).
func pyStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) })
}

// Encrypt returns the envelope form of plaintext for the configured provider.
func (e *Envelope) Encrypt(ctx context.Context, plaintext string) (string, error) {
	if e.provider == "vault_transit" {
		ct, err := e.vault.request(ctx, "encrypt", map[string]string{
			"plaintext": base64.StdEncoding.EncodeToString([]byte(plaintext))})
		if err != nil {
			return "", err
		}
		return VaultPrefix + ct, nil
	}
	if e.keyErr != nil {
		return "", e.keyErr
	}
	tok, err := e.fernet.Encrypt([]byte(plaintext))
	if err != nil {
		return "", err
	}
	return EncPrefix + tok, nil
}

// Decrypt reverses Encrypt. Empty -> ""; unprefixed (legacy plaintext) is
// returned as-is. Any local decrypt failure maps to ErrWrongKey without
// echoing the stored value.
func (e *Envelope) Decrypt(ctx context.Context, stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if strings.HasPrefix(stored, VaultPrefix) {
		enc, err := e.vault.request(ctx, "decrypt", map[string]string{"ciphertext": stored[len(VaultPrefix):]})
		if err != nil {
			return "", err
		}
		b, err := base64.StdEncoding.Strict().DecodeString(enc)
		if err != nil || !utf8.Valid(b) {
			return "", errors.New("Vault Transit returned invalid plaintext")
		}
		return string(b), nil
	}
	if !strings.HasPrefix(stored, EncPrefix) {
		return stored, nil
	}
	if e.keyErr != nil {
		return "", e.keyErr
	}
	b, err := e.fernet.Decrypt(stored[len(EncPrefix):])
	if err != nil || !utf8.Valid(b) {
		return "", ErrWrongKey
	}
	return string(b), nil
}
