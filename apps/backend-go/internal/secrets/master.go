package secrets

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

// ErrInsecureMasterKey is returned when no master key is configured and the
// SECRET_KEY is the publicly known default outside a dev environment.
var ErrInsecureMasterKey = errors.New("refusing to encrypt per-workspace secrets: SECRETS_MASTER_KEY is unset and SECRET_KEY is the insecure default outside a dev environment; set SECRETS_MASTER_KEY (a Fernet key) or a strong SECRET_KEY")

// MasterKeyBytes resolves the master key bytes exactly like Python
// _load_master_key: a non-empty (strip()ped) rawMasterKey that is a valid
// Fernet key is returned verbatim, any other non-empty value is hashed into a
// urlsafe-base64 sha256 key; with no master key the key is derived from
// secretKey, refused when secretKey is the insecure default and devEnv is
// false. It returns an error instead of panicking and never echoes key
// material in the error.
func MasterKeyBytes(rawMasterKey, secretKey string, insecureDefault, devEnv bool) ([]byte, error) {
	if raw := pyStrip(rawMasterKey); raw != "" {
		if _, err := ParseKey(raw); err == nil {
			return []byte(raw), nil
		}
		return deriveKeyBytes(raw), nil
	}
	if insecureDefault && !devEnv {
		return nil, ErrInsecureMasterKey
	}
	return deriveKeyBytes(secretKey), nil
}

func deriveKeyBytes(material string) []byte {
	sum := sha256.Sum256([]byte(material))
	return []byte(base64.URLEncoding.EncodeToString(sum[:]))
}
