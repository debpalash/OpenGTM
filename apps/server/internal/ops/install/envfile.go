package install

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// EnvFile is a .env file that keeps comments, order and unknown keys intact
// when a value changes, so `opengtm upgrade` can edit image tags without
// disturbing anything an operator added by hand.
type EnvFile struct {
	Path  string
	lines []string
}

// ReadEnv loads path. A missing file is returned as an error wrapping fs.ErrNotExist.
func ReadEnv(path string) (*EnvFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	e := &EnvFile{Path: path}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		e.lines = append(e.lines, sc.Text())
	}
	return e, sc.Err()
}

// NewEnv creates an empty in-memory file that Save will write to path.
func NewEnv(path string) *EnvFile { return &EnvFile{Path: path} }

func parseLine(line string) (key, val string, ok bool) {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "#") {
		return "", "", false
	}
	s = strings.TrimPrefix(s, "export ")
	k, v, found := strings.Cut(s, "=")
	if !found {
		return "", "", false
	}
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		v = v[1 : len(v)-1]
	}
	return strings.TrimSpace(k), v, true
}

// Get returns the last value assigned to key.
func (e *EnvFile) Get(key string) (string, bool) {
	var val string
	var found bool
	for _, l := range e.lines {
		if k, v, ok := parseLine(l); ok && k == key {
			val, found = v, true
		}
	}
	return val, found
}

// Value is Get without the found flag.
func (e *EnvFile) Value(key string) string { v, _ := e.Get(key); return v }

// Set replaces every assignment of key with one at the position of the first,
// or appends a new line.
func (e *EnvFile) Set(key, val string) {
	line := key + "=" + val
	out := e.lines[:0:0]
	done := false
	for _, l := range e.lines {
		if k, _, ok := parseLine(l); ok && k == key {
			if !done {
				out = append(out, line)
				done = true
			}
			continue
		}
		out = append(out, l)
	}
	if !done {
		out = append(out, line)
	}
	e.lines = out
}

// Comment appends a comment line.
func (e *EnvFile) Comment(text string) { e.lines = append(e.lines, "# "+text) }

// Blank appends an empty line.
func (e *EnvFile) Blank() { e.lines = append(e.lines, "") }

// Keys lists assigned keys in order of first appearance.
func (e *EnvFile) Keys() []string {
	seen := map[string]bool{}
	var keys []string
	for _, l := range e.lines {
		if k, _, ok := parseLine(l); ok && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	return keys
}

// Bytes renders the file.
func (e *EnvFile) Bytes() []byte {
	return []byte(strings.Join(e.lines, "\n") + "\n")
}

// Save writes the file atomically with mode 0600: it holds secrets.
func (e *EnvFile) Save() error { return writeFileAtomic(e.Path, e.Bytes(), 0o600) }

// writeFileAtomic writes via a temp file in the same directory and renames it,
// so a crash never leaves a truncated .env behind.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { os.Remove(name) }
	if err := tmp.Chmod(mode); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		cleanup()
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
