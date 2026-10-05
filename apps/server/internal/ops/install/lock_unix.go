//go:build unix

package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked means another opengtm operation holds the install lock.
var ErrLocked = errors.New("another opengtm init/upgrade/backup/restore is running on this install")

// Lock takes an exclusive advisory lock on the install directory so two
// operator commands (an upgrade and a cron backup, say) never interleave. The
// kernel drops it when the process exits, so a crash never leaves it stuck.
func (i *Install) Lock() (release func(), err error) {
	return lockFile(i.LockPath())
}

func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (lock file %s)", ErrLocked, path)
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
