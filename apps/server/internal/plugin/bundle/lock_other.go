//go:build !unix

package bundle

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// lockFile falls back to an exclusive-create lock file where flock is not
// available.
func lockFile(path string, wait time.Duration) (func(), error) {
	excl := path + ".excl"
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(excl, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return func() { os.Remove(excl) }, nil
		}
		if !errors.Is(err, os.ErrExist) || time.Now().After(deadline) {
			return nil, fmt.Errorf("connector installation is already in progress: %s", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
