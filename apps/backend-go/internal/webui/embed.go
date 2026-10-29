//go:build webui

package webui

import (
	"embed"
	"io/fs"
)

// The built UI (apps/web/dist) is copied to ./dist by `make build` / the api Dockerfile.
// Nothing under dist is committed.
//
//go:embed all:dist
var distFS embed.FS

// FS returns the embedded UI rooted at its index.html, or nil when dist holds no index.html.
func FS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
