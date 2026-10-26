//go:build integration

package poller_test

import "os"

func readFile(p string) ([]byte, error) { return os.ReadFile(p) }
