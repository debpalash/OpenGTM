package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ErrBadSignature is the verification failure of a signature that does not
// match the manifest bytes (VerifyResult.Err for SigInvalid).
var ErrBadSignature = errBadSignature

// Decode is manifest.load_manifest + canonical_manifest on one read of the
// bytes, without any signature check: the validated model, the sha256 of its
// canonical form (Entry.SHA256, never recomputed by callers) and the authored
// order of response.mappings. label is the path shown in the "is not a mapping"
// error (Python: str(path)).
func Decode(data []byte, label string) (Entry, error) {
	m, canonical, keys, err := decodeManifest(data)
	if err != nil {
		if err == errNotMapping {
			return Entry{}, fmt.Errorf("manifest %s is not a mapping", label)
		}
		return Entry{}, err
	}
	sum := sha256.Sum256(canonical)
	return Entry{Manifest: m, SHA256: hex.EncodeToString(sum[:]), MappingKeys: keys}, nil
}
