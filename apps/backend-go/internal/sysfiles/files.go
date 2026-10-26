// Package sysfiles implements the data-directory file operations and system stats behind
// /api/files/* and /api/system/stats. Hardened mode (default) confines every operation to the
// resolved data directory; legacy mode reproduces the Python behaviour (string-prefix check,
// recursive delete) for contract diffing only.
package sysfiles

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

var (
	// ErrAccessDenied: the path leaves the data directory (or is malformed).
	ErrAccessDenied = errors.New("access denied")
	// ErrNotFound: the target does not exist (or is not a regular file for Open).
	ErrNotFound = errors.New("not found")
	// ErrNotEmpty: hardened delete refuses a non-empty directory (no recursive delete).
	ErrNotEmpty = errors.New("directory is not empty")
	// ErrRoot: hardened delete refuses the data directory itself.
	ErrRoot = errors.New("cannot delete the data directory")
)

// Entry is one /api/files/list item (JSON key order matches Python's dict).
type Entry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	IsDir    bool   `json:"is_dir"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
	Type     string `json:"type"`
}

// DefaultListLimit caps hardened listings; legacy is unbounded.
const DefaultListLimit = 5000

// FS is a data directory plus the mode.
type FS struct {
	dir       string
	legacy    bool
	listLimit int
}

// New returns an FS over dataDir. legacy selects the Python-parity behaviour.
func New(dataDir string, legacy bool) *FS {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = filepath.Clean(dataDir)
	}
	return &FS{dir: abs, legacy: legacy, listLimit: DefaultListLimit}
}

// FileType buckets a file name like file_manager.get_file_type.
func FileType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".mkv", ".webm", ".mov":
		return "video"
	case ".mp3", ".wav", ".aac", ".ogg":
		return "audio"
	case ".pdf", ".epub", ".mobi", ".txt", ".md":
		return "document"
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		return "image"
	}
	return "other"
}

// evalPartial resolves symlinks on the longest existing prefix of p and appends the rest.
// The remainder never contains ".." (callers reject dot-dot segments before joining).
func evalPartial(p string) (string, error) {
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if rest == "" {
				return real, nil
			}
			return filepath.Join(real, rest), nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			// The component exists but does not resolve: a dangling symlink. Its target is
			// unknown (it could be created outside the data dir later), so refuse it.
			if _, lerr := os.Lstat(cur); lerr == nil {
				return "", ErrAccessDenied
			}
		}
		if !errors.Is(err, fs.ErrNotExist) {
			// Dangling links, loops and permission problems: treat the remainder as unresolvable.
			var pe *fs.PathError
			if errors.As(err, &pe) && errors.Is(pe.Err, syscall.ENOTDIR) {
				return "", ErrNotFound
			}
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func inside(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// hardenedPath resolves rel against the data dir. followLeaf=false keeps a final symlink
// unresolved (delete removes the link itself, never its target).
func (f *FS) hardenedPath(rel string, followLeaf bool) (root, target string, err error) {
	if strings.ContainsRune(rel, 0) || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return "", "", ErrAccessDenied
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == ".." {
			return "", "", ErrAccessDenied
		}
	}
	root, err = evalPartial(f.dir)
	if err != nil {
		return "", "", err
	}
	joined := filepath.Join(root, filepath.Clean("/"+rel)) // Clean("/"+rel) cannot climb above root
	if followLeaf {
		target, err = evalPartial(joined)
	} else if joined == root {
		target = root
	} else {
		var parent string
		if parent, err = evalPartial(filepath.Dir(joined)); err == nil {
			target = filepath.Join(parent, filepath.Base(joined))
		}
	}
	if err != nil {
		return "", "", err
	}
	if !inside(root, target) {
		return "", "", ErrAccessDenied
	}
	return root, target, nil
}

// legacyPath is the Python check: abspath(join(DATA_DIR, rel)).startswith(abspath(DATA_DIR)).
func (f *FS) legacyPath(rel string) (string, error) {
	var p string
	if filepath.IsAbs(rel) {
		p = filepath.Clean(rel)
	} else {
		p = filepath.Clean(filepath.Join(f.dir, rel))
	}
	if !strings.HasPrefix(p, f.dir) {
		return "", ErrAccessDenied
	}
	return p, nil
}

func isoLocal(t time.Time) string {
	t = t.Local().Round(time.Microsecond)
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s
}

// WithListLimit overrides the hardened listing cap (tests).
func (f *FS) WithListLimit(n int) *FS { c := *f; c.listLimit = n; return &c }

// List mirrors file_manager.list_files. Python quirk kept: files sort BEFORE folders
// (reverse=True on (not is_dir, modified)), each group by modified descending.
func (f *FS) List(rel string) ([]Entry, error) {
	l, _, err := f.ListTruncated(rel)
	return l, err
}

// ListTruncated is List plus a flag that is true when the hardened cap cut the result
// (the newest entries within each group are kept).
func (f *FS) ListTruncated(rel string) ([]Entry, bool, error) {
	var root, dir string
	var err error
	if f.legacy {
		root = f.dir
		if dir, err = f.legacyPath(rel); err != nil {
			return nil, false, err
		}
	} else if root, dir, err = f.hardenedPath(rel, true); err != nil {
		return nil, false, err
	}
	st, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return []Entry{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !st.IsDir() {
		return nil, false, &fs.PathError{Op: "scandir", Path: dir, Err: syscall.ENOTDIR}
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, false, err
	}
	out := []Entry{}
	for _, de := range des {
		if strings.HasPrefix(de.Name(), ".") {
			continue
		}
		full := filepath.Join(dir, de.Name())
		if !f.legacy && de.Type()&fs.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(full)
			if err != nil || !inside(root, real) {
				continue // dangling or escaping links are not listed
			}
		}
		info, err := os.Stat(full) // follows symlinks, like DirEntry.stat()
		if err != nil {
			if f.legacy {
				return nil, false, err
			}
			continue
		}
		e := Entry{Name: de.Name(), IsDir: info.IsDir(), Modified: isoLocal(info.ModTime())}
		if e.Path, err = filepath.Rel(root, full); err != nil {
			continue
		}
		if e.IsDir {
			e.Type = "folder"
		} else {
			e.Size, e.Type = info.Size(), FileType(de.Name())
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return !out[i].IsDir
		}
		return out[i].Modified > out[j].Modified
	})
	if !f.legacy && f.listLimit > 0 && len(out) > f.listLimit {
		return out[:f.listLimit], true, nil
	}
	return out, false, nil
}

// Open resolves rel to a regular file for download.
func (f *FS) Open(rel string) (string, os.FileInfo, error) {
	var p string
	var err error
	if f.legacy {
		p, err = f.legacyPath(rel)
	} else {
		_, p, err = f.hardenedPath(rel, true)
	}
	if err != nil {
		return "", nil, err
	}
	st, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return "", nil, ErrNotFound
		}
		return "", nil, err
	}
	if !st.Mode().IsRegular() {
		return "", nil, ErrNotFound
	}
	return p, st, nil
}

// OpenFile opens rel for reading. Hardened mode opens with O_NOFOLLOW (the resolved path has
// no symlinks left, so a link at the leaf means a swap) and re-checks, through /proc/self/fd,
// that the opened file still lives inside the data dir.
func (f *FS) OpenFile(rel string) (*os.File, os.FileInfo, error) {
	p, _, err := f.Open(rel)
	if err != nil {
		return nil, nil, err
	}
	flags := os.O_RDONLY
	if !f.legacy {
		flags |= syscall.O_NOFOLLOW
	}
	fh, err := os.OpenFile(p, flags, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, nil, ErrAccessDenied
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}
	st, err := fh.Stat()
	if err == nil && !st.Mode().IsRegular() {
		err = ErrNotFound
	}
	if err == nil && !f.legacy {
		if root, rerr := evalPartial(f.dir); rerr != nil {
			err = rerr
		} else if real, rerr := os.Readlink("/proc/self/fd/" + fmt.Sprint(fh.Fd())); rerr == nil && !inside(root, real) {
			err = ErrAccessDenied
		}
	}
	if err != nil {
		fh.Close()
		return nil, nil, err
	}
	return fh, st, nil
}

// Delete removes a file, symlink (the link only) or empty directory. Legacy mode removes
// directories recursively like shutil.rmtree.
func (f *FS) Delete(rel string) error {
	if f.legacy {
		p, err := f.legacyPath(rel)
		if err != nil {
			return err
		}
		st, err := os.Stat(p)
		if err != nil {
			return ErrNotFound
		}
		if st.IsDir() {
			return os.RemoveAll(p)
		}
		return os.Remove(p)
	}
	root, p, err := f.hardenedPath(rel, false)
	if err != nil {
		return err
	}
	if p == root {
		return ErrRoot
	}
	st, err := os.Lstat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return ErrNotFound
		}
		return err
	}
	// Narrow the resolve-then-remove window: the parent must still resolve to the same place.
	if np, perr := evalPartial(filepath.Dir(p)); perr != nil || np != filepath.Dir(p) {
		return ErrAccessDenied
	}
	if st.IsDir() {
		if err := os.Remove(p); err != nil {
			if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
				return ErrNotEmpty
			}
			return err
		}
		return nil
	}
	return os.Remove(p)
}

// Stat is the /api/system/stats body.
type Stat struct {
	DiskTotal   uint64 `json:"disk_total"`
	DiskUsed    uint64 `json:"disk_used"`
	DiskFree    uint64 `json:"disk_free"`
	ProjectSize int64  `json:"project_size"`
	DBSize      int64  `json:"db_size"`
	// ProjectSizePartial is set (hardened mode only, key otherwise absent) when the walk hit the
	// entry or time cap, so project_size is a lower bound.
	ProjectSizePartial bool `json:"project_size_partial,omitempty"`
}

// StatsLimits bounds the project-size walk. Zero values mean unbounded (legacy parity).
type StatsLimits struct {
	MaxEntries int
	MaxTime    time.Duration
}

// Stats mirrors get_system_stats: statfs of diskPath (shutil.disk_usage), a walk of
// projectRoot that skips symlinks, and the size of the SQLite file named by databaseURL
// when it starts with sqlite:/// (else 0).
func Stats(ctx context.Context, diskPath, projectRoot, databaseURL string, lim ...StatsLimits) (Stat, error) {
	var l StatsLimits
	if len(lim) > 0 {
		l = lim[0]
	}
	start, seen := time.Now(), 0
	var s Stat
	var sf syscall.Statfs_t
	if err := syscall.Statfs(diskPath, &sf); err != nil {
		return s, err
	}
	bs := uint64(sf.Frsize)
	if bs == 0 {
		bs = uint64(sf.Bsize)
	}
	s.DiskTotal = sf.Blocks * bs
	s.DiskUsed = (sf.Blocks - sf.Bfree) * bs
	s.DiskFree = sf.Bavail * bs
	_ = filepath.WalkDir(projectRoot, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		seen++
		if (l.MaxEntries > 0 && seen > l.MaxEntries) || (l.MaxTime > 0 && seen%256 == 0 && time.Since(start) > l.MaxTime) {
			s.ProjectSizePartial = true
			return fs.SkipAll
		}
		if err != nil || d.Type()&fs.ModeSymlink != 0 {
			return nil // unreadable entries and symlinks (files or dirs) are skipped
		}
		if !d.IsDir() {
			if info, ierr := d.Info(); ierr == nil && info.Mode().IsRegular() {
				s.ProjectSize += info.Size()
			}
		}
		return nil
	})
	if err := ctx.Err(); err != nil {
		return s, err
	}
	if path, ok := strings.CutPrefix(databaseURL, "sqlite:///"); ok {
		if st, err := os.Stat(path); err == nil {
			s.DBSize = st.Size()
		}
	}
	return s, nil
}
