//go:build !unix

package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrLocked means another opengtm operation holds the install lock.
var ErrLocked = errors.New("another opengtm init/upgrade/backup/restore is running on this install")

// Lock uses an exclusive-create lock file where flock is unavailable. A
// crashed run can leave it behind; the error names the file so the operator
// can remove it once they have confirmed nothing else is running.
func (i *Install) Lock() (release func(), err error) {
	path := i.LockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w (remove %s if no other run is active)", ErrLocked, path)
		}
		return nil, err
	}
	f.Close()
	return func() { os.Remove(path) }, nil
}
