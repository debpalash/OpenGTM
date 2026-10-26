package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// Decode is manifest.load_manifest + canonical_manifest on one read of the
// bytes, without any signature check: the validated model, the sha256 of its
// canonical form (Entry.SHA256, never recomputed by callers) and the authored
// order of response.mappings. label is the path shown in the "is not a mapping"
// error (Python: str(path)).
// ErrBadSignature is the verification failure of a signature that does not
// match the manifest bytes (VerifyResult.Err for SigInvalid).
var ErrBadSignature = errBadSignature

func Decode(data []byte, label string) (Entry, error) {
	m, canonical, err := decodeManifest(data)
	if err != nil {
		if err == errNotMapping {
			return Entry{}, fmt.Errorf("manifest %s is not a mapping", label)
		}
		return Entry{}, err
	}
	sum := sha256.Sum256(canonical)
	switch m.Auth.Type {
	case "header", "bearer", "query", "none":
	default:
		return Entry{}, errors.New("1 validation error for ProviderManifest\nauth.type\n  Input should be 'header', 'bearer', 'query' or 'none'")
	}
	e := Entry{Manifest: m, SHA256: hex.EncodeToString(sum[:])}
	if tree, err := loadYAML(data); err == nil {
		if root, ok := tree.(*pyMap); ok {
			if resp, ok := pyGet(root, "response"); ok {
				if rm, ok := resp.(*pyMap); ok {
					if mp, ok := pyGet(rm, "mappings"); ok {
						if mm, ok := mp.(*pyMap); ok {
							for _, ent := range mm.ents {
								if k, ok := ent.key.(string); ok {
									e.MappingKeys = append(e.MappingKeys, k)
								}
							}
						}
					}
				}
			}
		}
	}
	return e, nil
}
