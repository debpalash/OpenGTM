package secrets

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Envelope prefixes. v1 is the local Fernet envelope shared with Python;
// the Vault Transit envelope is recognized only to refuse it clearly.
const (
	EnvelopePrefix = "enc:v1:"
	vaultPrefix    = "enc:v2:vault:"
)

// insecureDefaultSecretKey is the shipped development SECRET_KEY
// (apps/api/core/config.py INSECURE_DEFAULT_SECRET_KEY).
const insecureDefaultSecretKey = "INSECURE_FALLBACK_KEY_FOR_DEVELOPMENT_ONLY"

// Errors callers can match.
var (
	// ErrNoKey means no usable encryption key is configured and the
	// deployment is not a development one.
	ErrNoKey = errors.New("secrets: no encryption key: set SECRETS_MASTER_KEY (a Fernet key) or a strong SECRET_KEY")
	// ErrUnsupportedProvider means SECRETS_PROVIDER is not "local", or a
	// stored value uses the Vault Transit envelope.
	ErrUnsupportedProvider = errors.New("secrets: only the local (Fernet) secrets provider is supported by this host")
)

// devEnvs are the APP_ENV values that tolerate the insecure default key
// (apps/api/core/config.py _DEV_ENVS).
var devEnvs = map[string]bool{"dev": true, "development": true, "test": true, "testing": true, "local": true}

// Cipher encrypts and decrypts secret values in the shared envelope.
type Cipher struct {
	key Key
	now func() time.Time
}

// NewCipher builds a Cipher from a key.
func NewCipher(k Key) *Cipher { return &Cipher{key: k, now: time.Now} }

// FromEnv resolves the key exactly as the Python app does
// (workspace/secrets.py _load_master_key):
//
//  1. SECRETS_MASTER_KEY, stripped: used verbatim when it is a valid Fernet
//     key, otherwise hashed into one.
//  2. Otherwise SECRET_KEY (default the insecure development key), hashed.
//     This fails closed unless APP_ENV is a development environment when
//     SECRET_KEY is still the insecure default.
//
// SECRETS_PROVIDER must be "local" (the default).
func FromEnv(lookup func(string) (string, bool)) (*Cipher, error) {
	get := func(name, def string) string {
		if v, ok := lookup(name); ok {
			return v
		}
		return def
	}
	if p := strings.ToLower(strings.TrimSpace(get("SECRETS_PROVIDER", "local"))); p != "local" {
		return nil, fmt.Errorf("%w (SECRETS_PROVIDER=%q)", ErrUnsupportedProvider, p)
	}
	if raw := strings.TrimSpace(get("SECRETS_MASTER_KEY", "")); raw != "" {
		if k, err := ParseKey(raw); err == nil {
			return NewCipher(k), nil
		}
		return NewCipher(DeriveKey(raw)), nil
	}
	secret := get("SECRET_KEY", insecureDefaultSecretKey)
	appEnv := strings.ToLower(strings.TrimSpace(get("APP_ENV", "dev")))
	if strings.Contains(secret, insecureDefaultSecretKey) && !devEnvs[appEnv] {
		return nil, ErrNoKey
	}
	return NewCipher(DeriveKey(secret)), nil
}

// Encrypt returns the stored envelope for plaintext.
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	tok, err := c.key.Encrypt([]byte(plaintext), c.now())
	if err != nil {
		return "", err
	}
	return EnvelopePrefix + tok, nil
}

// Decrypt reads a stored envelope. Unlike the Python helper it never passes
// unprefixed text through as plaintext: every value this host stores is
// encrypted, so anything else is a corrupted or foreign row.
func (c *Cipher) Decrypt(stored string) (string, error) {
	switch {
	case strings.HasPrefix(stored, vaultPrefix):
		return "", ErrUnsupportedProvider
	case !strings.HasPrefix(stored, EnvelopePrefix):
		return "", errors.New("secrets: stored value is not in the enc:v1 envelope")
	}
	pt, err := c.key.Decrypt(strings.TrimPrefix(stored, EnvelopePrefix))
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
