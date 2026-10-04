// Package signing ports apps/api/services/leadgen/enrichment/declarative/
// signing.py: detached Ed25519 signatures over the canonical form of a
// manifest, the trusted-publisher store, and signature policies.
//
// Canonical form (byte-for-byte identical to Python):
//
//	json.dumps(yaml.safe_load(text), sort_keys=True, separators=(",", ":"),
//	           ensure_ascii=False).encode("utf-8")
//
// Envelope (<manifest>.sig), written as json.dumps(..., sort_keys=True,
// indent=2) + "\n":
//
//	{"algorithm": "Ed25519", "key_id": "...", "manifest_sha256": "<hex>",
//	 "signature": "<base64>", "signature_version": "1"}
//
// Trust store: {"version": 1, "keys": [{"key_id", "publisher", "public_key"
// (base64 raw 32-byte Ed25519 key)}]}.
package signing

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
)

// SignatureVersion is the only supported envelope version.
const SignatureVersion = "1"

// Statuses returned by VerifyManifest.
const (
	StatusUnsigned  = "unsigned"
	StatusUntrusted = "untrusted"
	StatusInvalid   = "invalid"
	StatusTrusted   = "trusted"
)

// Policies. Python accepts "optional" and "required"; any other configured
// value fails closed to "required".
const (
	PolicyOptional = "optional"
	PolicyRequired = "required"
)

// NormalizePolicy maps an arbitrary policy string onto a supported policy
// exactly like the Python helpers (unknown values become "required"; the
// empty string means "use the default", which is "optional").
func NormalizePolicy(p string) string {
	switch p {
	case "":
		return PolicyOptional
	case PolicyOptional, PolicyRequired:
		return p
	}
	return PolicyRequired
}

// PolicyFromEnv mirrors _signature_policy(): CONNECTOR_SIGNATURE_POLICY,
// default "optional", stripped and lower-cased, unknown -> "required".
func PolicyFromEnv() string {
	v, ok := os.LookupEnv("CONNECTOR_SIGNATURE_POLICY")
	if !ok {
		return PolicyOptional
	}
	v = lower(pycompat.Strip(v))
	if v == PolicyOptional || v == PolicyRequired {
		return v
	}
	return PolicyRequired
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// Rejects reports whether a verification result fails the policy, mirroring
// `status in {"invalid","untrusted"} or (policy == "required" and status != "trusted")`.
func Rejects(policy string, r Result) bool {
	return r.Status == StatusInvalid || r.Status == StatusUntrusted ||
		(policy == PolicyRequired && r.Status != StatusTrusted)
}

// CanonicalBytes canonicalizes manifest text.
func CanonicalBytes(text []byte) ([]byte, error) {
	v, err := pycompat.LoadYAML(text)
	if err != nil {
		return nil, err
	}
	if _, ok := v.(*pycompat.Map); !ok {
		return nil, errors.New("manifest is not a mapping")
	}
	return pycompat.Canonical(v)
}

// CanonicalManifest reads and canonicalizes a manifest file.
func CanonicalManifest(path string) ([]byte, error) {
	text, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return CanonicalBytes(text)
}

// Digest returns the hex SHA-256 of the canonical manifest.
func Digest(path string) (string, error) {
	c, err := CanonicalManifest(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:]), nil
}

// TrustStore maps key ids (compared with Python equality) to entries.
type TrustStore struct{ keys *pycompat.Map }

// Entry returns the raw entry for a key id, if present.
func (t TrustStore) Entry(keyID any) (*pycompat.Map, bool, error) {
	if t.keys == nil {
		return nil, false, nil
	}
	if _, isList := keyID.([]any); isList {
		return nil, false, errors.New("unhashable type: 'list'")
	}
	if _, isMap := keyID.(*pycompat.Map); isMap {
		return nil, false, errors.New("unhashable type: 'dict'")
	}
	v, ok := t.keys.Get(keyID)
	if !ok {
		return nil, false, nil
	}
	m, _ := v.(*pycompat.Map)
	return m, true, nil
}

// LoadTrustStore mirrors load_trust_store: a missing file is an empty store.
func LoadTrustStore(path string) (TrustStore, error) {
	if path == "" {
		return TrustStore{}, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return TrustStore{}, nil
	}
	if err != nil {
		return TrustStore{}, err
	}
	raw, err := pycompat.LoadJSON(data)
	if err != nil {
		return TrustStore{}, err
	}
	m, ok := raw.(*pycompat.Map)
	if !ok {
		return TrustStore{}, fmt.Errorf("'%s' object has no attribute 'get'", pycompat.TypeName(raw))
	}
	version, _ := m.Get("version")
	keys, _ := m.Get("keys")
	list, isList := keys.([]any)
	if !pyEqualsOne(version) || !isList {
		return TrustStore{}, errors.New("invalid connector trust store")
	}
	out := pycompat.NewMap()
	for _, item := range list {
		im, ok := item.(*pycompat.Map)
		if !ok {
			if _, isStr := item.(string); isStr {
				return TrustStore{}, errors.New("string indices must be integers, not 'str'")
			}
			return TrustStore{}, fmt.Errorf("'%s' object is not subscriptable", pycompat.TypeName(item))
		}
		id, ok := im.Get("key_id")
		if !ok {
			return TrustStore{}, errors.New("'key_id'")
		}
		if err := out.Set(id, im); err != nil {
			return TrustStore{}, err
		}
	}
	return TrustStore{keys: out}, nil
}

// pyEqualsOne mirrors `value == 1` in Python (1, 1.0 and True all qualify).
func pyEqualsOne(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case int64:
		return t == 1
	case float64:
		return t == 1
	}
	return false
}

// Result mirrors the dict returned by verify_manifest.
type Result struct {
	Status    string `json:"status"`
	KeyID     any    `json:"key_id"`
	Publisher any    `json:"publisher"`
	Error     string `json:"error,omitempty"`
}

// PyValue renders the result as an ordered Python-style dict.
func (r Result) PyValue() *pycompat.Map {
	m := pycompat.NewMap()
	_ = m.Set("status", r.Status)
	_ = m.Set("key_id", r.KeyID)
	_ = m.Set("publisher", r.Publisher)
	if r.Status == StatusInvalid {
		_ = m.Set("error", r.Error)
	}
	return m
}

var b64Alphabet = regexp.MustCompile(`^[A-Za-z0-9+/=]*$`)

// b64Strict mirrors base64.b64decode(s, validate=True) for str input.
func b64Strict(v any) ([]byte, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("argument should be a bytes-like object or ASCII string, not '%s'", pycompat.TypeName(v))
	}
	if !b64Alphabet.MatchString(s) {
		return nil, errors.New("Only base64 data is allowed")
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("Incorrect padding")
	}
	return b, nil
}

// VerifyManifest mirrors verify_manifest(path, trust_store_path).
func VerifyManifest(path, trustStorePath string) Result {
	sigPath := path + ".sig"
	if _, err := os.Stat(sigPath); err != nil {
		return Result{Status: StatusUnsigned}
	}
	var keyID any
	invalid := func(err error) Result {
		return Result{Status: StatusInvalid, KeyID: keyID, Error: err.Error()}
	}
	data, err := os.ReadFile(sigPath)
	if err != nil {
		return invalid(err)
	}
	rawEnv, err := pycompat.LoadJSON(data)
	if err != nil {
		return invalid(err)
	}
	env, ok := rawEnv.(*pycompat.Map)
	if !ok {
		return invalid(fmt.Errorf("'%s' object has no attribute 'get'", pycompat.TypeName(rawEnv)))
	}
	keyID, _ = env.Get("key_id")
	if v, _ := env.Get("signature_version"); v != SignatureVersion {
		return invalid(errors.New("unsupported connector signature envelope"))
	}
	if v, _ := env.Get("algorithm"); v != "Ed25519" {
		return invalid(errors.New("unsupported connector signature envelope"))
	}
	store, err := LoadTrustStore(trustStorePath)
	if err != nil {
		return invalid(err)
	}
	entry, found, err := store.Entry(keyID)
	if err != nil {
		return invalid(err)
	}
	if !found {
		return Result{Status: StatusUntrusted, KeyID: keyID}
	}
	payload, err := CanonicalManifest(path)
	if err != nil {
		return invalid(err)
	}
	sum := sha256.Sum256(payload)
	if want, _ := env.Get("manifest_sha256"); want != hex.EncodeToString(sum[:]) {
		return invalid(errors.New("manifest digest mismatch"))
	}
	if entry == nil {
		return invalid(errors.New("trust store entry is not a mapping"))
	}
	pkRaw, ok := entry.Get("public_key")
	if !ok {
		return invalid(errors.New("'public_key'"))
	}
	pk, err := b64Strict(pkRaw)
	if err != nil {
		return invalid(err)
	}
	if len(pk) != ed25519.PublicKeySize {
		return invalid(errors.New("An Ed25519 public key is 32 bytes long"))
	}
	sigRaw, ok := env.Get("signature")
	if !ok {
		return invalid(errors.New("'signature'"))
	}
	sig, err := b64Strict(sigRaw)
	if err != nil {
		return invalid(err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pk), payload, sig) {
		return invalid(errors.New(""))
	}
	publisher, ok := entry.Get("publisher")
	if !ok {
		publisher = keyID
	}
	return Result{Status: StatusTrusted, KeyID: keyID, Publisher: publisher}
}

// LoadPrivateKey reads a PKCS#8 PEM Ed25519 private key.
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("could not deserialize key data: no PEM block found")
	}
	if block.Type == "ENCRYPTED PRIVATE KEY" {
		return nil, errors.New("encrypted private keys are not supported (Python loads with password=None)")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("could not deserialize key data: %w", err)
	}
	ek, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("connector signing key must be Ed25519")
	}
	return ek, nil
}

// EncodePrivateKey returns a PKCS#8 PEM encoding of key.
func EncodePrivateKey(key ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// Envelope builds the detached signature JSON for a canonical payload.
func Envelope(payload []byte, key ed25519.PrivateKey, keyID string) ([]byte, error) {
	sum := sha256.Sum256(payload)
	env := pycompat.NewMap()
	_ = env.Set("signature_version", SignatureVersion)
	_ = env.Set("algorithm", "Ed25519")
	_ = env.Set("key_id", keyID)
	_ = env.Set("manifest_sha256", hex.EncodeToString(sum[:]))
	_ = env.Set("signature", base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload)))
	out, err := pycompat.Dumps(env, pycompat.DumpOptions{SortKeys: true, Indent: 2, EnsureASCII: true})
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// SignManifest mirrors sign_manifest: writes <path>.sig and returns its path.
func SignManifest(path, privateKeyPath, keyID string) (string, error) {
	key, err := LoadPrivateKey(privateKeyPath)
	if err != nil {
		return "", err
	}
	return SignManifestWithKey(path, key, keyID)
}

// SignManifestWithKey signs with an in-memory key.
func SignManifestWithKey(path string, key ed25519.PrivateKey, keyID string) (string, error) {
	payload, err := CanonicalManifest(path)
	if err != nil {
		return "", err
	}
	env, err := Envelope(payload, key, keyID)
	if err != nil {
		return "", err
	}
	out := path + ".sig"
	if err := os.WriteFile(out, env, 0o644); err != nil {
		return "", err
	}
	return out, nil
}

// TrustEntry returns a trust-store entry JSON object for a public key.
func TrustEntry(keyID, publisher string, pub ed25519.PublicKey) map[string]string {
	return map[string]string{
		"key_id":     keyID,
		"publisher":  publisher,
		"public_key": base64.StdEncoding.EncodeToString(pub),
	}
}
