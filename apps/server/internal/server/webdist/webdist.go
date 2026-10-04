// Package webdist embeds the built React dashboard into the opengtm binary.
//
// The repository only carries a placeholder. Image and release builds copy
// the Vite output in before compiling:
//
//	bun run --cwd apps/web build
//	cp -a apps/web/dist/. apps/server/internal/server/webdist/dist/
//	go build ./apps/server/cmd/opengtm
//
// A binary built without the assets still runs; it serves the UI from
// OPENGTM_WEB_DIR when set, or answers UI routes with a clear 404.
package webdist

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the embedded dist directory as the web root.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // the directory is part of this package; cannot happen
	}
	return sub
}
