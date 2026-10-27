// Package testutil holds helpers shared by tests.
package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// moduleRoot is the repo root: apps/backend-go/internal/testutil -> four levels up.
func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
}

// LegacyPythonDir returns the absolute path of legacy/python (the local-only,
// gitignored Python tree). It skips the calling test when the tree is absent, so
// parity checks against Python sources only run where those sources exist.
func LegacyPythonDir(t testing.TB) string {
	t.Helper()
	d := filepath.Join(repoRoot(), "legacy", "python")
	if st, err := os.Stat(d); err != nil || !st.IsDir() {
		t.Skipf("legacy Python tree not present (%s): parity check skipped", d)
	}
	return d
}

// LegacyPath is LegacyPythonDir joined with rel; it skips when the tree or the file is absent.
func LegacyPath(t testing.TB, rel ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{LegacyPythonDir(t)}, rel...)...)
	if _, err := os.Stat(p); err != nil {
		t.Skipf("legacy Python file not present (%s): parity check skipped", p)
	}
	return p
}
