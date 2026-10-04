//go:build unix

package bundle

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// lockFile holds a cross-process exclusive flock on path, like the Python
// _install_lock (the file is created with a single "0" byte if empty).
func lockFile(path string, wait time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	if info, err := f.Stat(); err == nil && info.Size() == 0 {
		_, _ = f.Write([]byte("0"))
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			f.Close()
			name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "."), ".install.lock")
			return nil, fmt.Errorf("connector installation is already in progress: %s", "."+name+".install.lock")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
