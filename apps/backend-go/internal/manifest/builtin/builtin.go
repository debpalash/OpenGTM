// Package builtin ships the connector manifests and the default trust store inside
// the Go binaries (go:embed), so the catalog and the declarative workbook providers
// work in images and CI where no repository checkout exists.
//
// Files are byte copies of legacy/python/.../declarative/manifests and
// docs/connectors/trusted-publishers.json (builtin_test.go keeps them identical).
// They are written to a private temp directory once and read back through the same
// disk paths as operator-supplied files, so signature, symlink and certification
// handling is identical for both sources.
package builtin

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

//go:embed manifests trusted-publishers.json
var files embed.FS

// FS is the embedded tree: manifests/** and trusted-publishers.json.
func FS() fs.FS { return files }

var (
	once     sync.Once
	dir      string
	trust    string
	matError error
)

// Paths materialises the embedded files on first use and returns the manifest
// directory and the trust store file. The directory lives for the process lifetime.
func Paths() (manifestsDir, trustStore string, err error) {
	once.Do(func() {
		root, e := os.MkdirTemp("", "opengtm-builtin-")
		if e != nil {
			matError = e
			return
		}
		matError = fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			out := filepath.Join(root, filepath.FromSlash(p))
			if d.IsDir() {
				return os.MkdirAll(out, 0o700)
			}
			b, err := files.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(out, b, 0o400)
		})
		if matError != nil {
			_ = os.RemoveAll(root)
			return
		}
		dir, trust = filepath.Join(root, "manifests"), filepath.Join(root, "trusted-publishers.json")
	})
	return dir, trust, matError
}
