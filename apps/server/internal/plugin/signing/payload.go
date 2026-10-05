package signing

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
)

// SignPayload signs raw bytes (not a canonicalized manifest) and returns the
// detached envelope, in the same format as a manifest signature. The
// "manifest_sha256" field then holds the SHA-256 of the payload. The plugin
// index is signed this way: the signature covers the exact bytes of
// index.json, so byte-identical mirrors verify identically.
func SignPayload(payload []byte, key ed25519.PrivateKey, keyID string) ([]byte, error) {
	return Envelope(payload, key, keyID)
}

// VerifyPayload verifies a detached envelope over raw payload bytes against
// the trust store. It follows VerifyManifest's decision order and statuses
// (invalid, untrusted, trusted) but never reports "unsigned": a caller with
// no envelope should not call it.
func VerifyPayload(payload, envelope []byte, trustStorePath string) Result {
	var keyID any
	invalid := func(err error) Result {
		return Result{Status: StatusInvalid, KeyID: keyID, Error: err.Error()}
	}
	rawEnv, err := pycompat.LoadJSON(envelope)
	if err != nil {
		return invalid(err)
	}
	env, ok := rawEnv.(*pycompat.Map)
	if !ok {
		return invalid(fmt.Errorf("signature envelope is a %s, not an object", pycompat.TypeName(rawEnv)))
	}
	keyID, _ = env.Get("key_id")
	if v, _ := env.Get("signature_version"); v != SignatureVersion {
		return invalid(errors.New("unsupported signature envelope"))
	}
	if v, _ := env.Get("algorithm"); v != "Ed25519" {
		return invalid(errors.New("unsupported signature envelope"))
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
	sum := sha256.Sum256(payload)
	if want, _ := env.Get("manifest_sha256"); want != hex.EncodeToString(sum[:]) {
		return invalid(errors.New("payload digest mismatch"))
	}
	if entry == nil {
		return invalid(errors.New("trust store entry is not a mapping"))
	}
	pkRaw, ok := entry.Get("public_key")
	if !ok {
		return invalid(errors.New("trust store entry has no public_key"))
	}
	pk, err := b64Strict(pkRaw)
	if err != nil {
		return invalid(err)
	}
	if len(pk) != ed25519.PublicKeySize {
		return invalid(errors.New("an Ed25519 public key is 32 bytes long"))
	}
	sigRaw, ok := env.Get("signature")
	if !ok {
		return invalid(errors.New("signature envelope has no signature"))
	}
	sig, err := b64Strict(sigRaw)
	if err != nil {
		return invalid(err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pk), payload, sig) {
		return invalid(errors.New("signature does not match"))
	}
	publisher, ok := entry.Get("publisher")
	if !ok {
		publisher = keyID
	}
	return Result{Status: StatusTrusted, KeyID: keyID, Publisher: publisher}
}
