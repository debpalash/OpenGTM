//go:build !webui

package webui

import "io/fs"

// FS is nil without the webui build tag: the UI handler answers 404 "UI not built".
func FS() fs.FS { return nil }
