package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/signing"
)

// Info is what a v2 bundle claims about itself, read without installing it.
// Nothing here is trusted: the signature is not verified by Inspect.
type Info struct {
	Plugin *manifest.Plugin
	// SHA256 and Size describe the bundle file itself.
	SHA256 string
	Size   int64
	// ManifestSHA256 is the hex SHA-256 of the canonical manifest.
	ManifestSHA256 string
	// Canonical is the canonical manifest the signature covers.
	Canonical []byte
	// Envelope is the raw plugin.yaml.sig.
	Envelope []byte
}

// Inspect reads a v2 plugin bundle's manifest and signature envelope under
// the same size limits as Install. v1 connector bundles are refused.
func Inspect(bundlePath string) (*Info, error) {
	raw, err := os.ReadFile(bundlePath)
	if err != nil {
		return nil, err
	}
	files, _, err := readEntries(bundlePath, V2MaxFileBytes, V2MaxTotalBytes, V2MaxEntries)
	if err != nil {
		return nil, err
	}
	if files[v2Manifest] == nil {
		if files[v1Manifest] != nil {
			return nil, ErrVerifyV1
		}
		return nil, errors.New("plugin bundle must contain plugin.yaml and plugin.yaml.sig")
	}
	if files[v2Signature] == nil {
		return nil, errors.New("plugin bundle is not signed (no plugin.yaml.sig)")
	}
	p, err := manifest.LoadBytes(v2Manifest, files[v2Manifest])
	if err != nil {
		return nil, err
	}
	if p.ManifestVersion != "2" {
		return nil, errors.New("plugin.yaml must be manifest_version 2")
	}
	canon, err := signing.CanonicalBytes(files[v2Manifest])
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	msum := sha256.Sum256(canon)
	return &Info{
		Plugin: p, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(raw)),
		ManifestSHA256: hex.EncodeToString(msum[:]), Canonical: canon, Envelope: files[v2Signature],
	}, nil
}

var pluginNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{2,63}$`)

// Remove deletes an installed v2 plugin directory. The directory is renamed
// out of the way under the install lock before it is deleted, so a loader
// never observes a half-removed plugin.
func Remove(destination, name string) error {
	if !pluginNameRE.MatchString(name) {
		return fmt.Errorf("invalid plugin name %q", name)
	}
	target := filepath.Join(destination, name)
	if !exists(target) {
		return fmt.Errorf("plugin %s is not installed", name)
	}
	p, err := manifest.Load(filepath.Join(target, v2Manifest))
	if err != nil {
		return fmt.Errorf("%s does not hold an installed plugin (%v); remove it by hand", target, err)
	}
	if p.Name != name {
		return fmt.Errorf("%s holds plugin %q, not %q; remove it by hand", target, p.Name, name)
	}
	unlock, err := lockFile(filepath.Join(destination, "."+name+".install.lock"), InstallLockWait)
	if err != nil {
		return err
	}
	defer unlock()
	trash := filepath.Join(destination, fmt.Sprintf(".%s.%s.removed", name, randHex(8)))
	if err := os.Rename(target, trash); err != nil {
		return err
	}
	return os.RemoveAll(trash)
}
