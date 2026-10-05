//go:build unix

package backup

import (
	"archive/tar"
	"os"
)

// chown restores ownership recorded in the archive when running as root (the
// data directory is shared with containers that run as another uid). As an
// ordinary user the files simply belong to that user.
func chown(path string, hdr *tar.Header) {
	if os.Geteuid() == 0 {
		_ = os.Lchown(path, hdr.Uid, hdr.Gid)
	}
}
