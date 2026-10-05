// Package backup creates, verifies and restores OpenGTM backups.
//
// A backup is a directory:
//
//	manifest.json   what it holds, versions, and a SHA-256 for every file
//	db.dump         pg_dump --format=custom of the application database
//	roles.sql       idempotent CREATE ROLE / GRANT script (no passwords)
//	data.tar.gz     the data directory (workspace metadata, files)
//	config.tar.gz   .env, opengtm.yaml and compose.yml (secrets; mode 0600)
//
// Everything is verified against the manifest before a restore touches
// anything, and a database restore runs in one transaction.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/ops/pg"
)

// FormatVersion is bumped when the layout changes incompatibly.
const FormatVersion = 1

// File names inside a backup directory.
const (
	ManifestFile = "manifest.json"
	DumpFile     = "db.dump"
	RolesFile    = "roles.sql"
	DataFile     = "data.tar.gz"
	ConfigFile   = "config.tar.gz"
)

// Manifest describes a backup and lets restore verify it before acting.
type Manifest struct {
	Format           int         `json:"format"`
	CreatedAt        time.Time   `json:"created_at"`
	OpenGTMVersion   string      `json:"opengtm_version"`
	Profile          string      `json:"profile,omitempty"`
	Label            string      `json:"label,omitempty"`
	PostgresVersion  string      `json:"postgres_version"`
	Database         string      `json:"database"`
	AlembicRevisions []string    `json:"alembic_revisions"`
	Tables           int         `json:"tables"`
	Files            []FileEntry `json:"files"`
	// Skipped lists data-directory entries that were not archived
	// (symlinks, sockets, devices), so nothing is dropped silently.
	Skipped []string `json:"skipped,omitempty"`
}

// FileEntry is one file with its checksum.
type FileEntry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// File returns the entry for name.
func (m Manifest) File(name string) (FileEntry, bool) {
	for _, f := range m.Files {
		if f.Name == name {
			return f, true
		}
	}
	return FileEntry{}, false
}

// CreateOptions configures Create.
type CreateOptions struct {
	// Dir is the parent directory; the backup goes in a new subdirectory.
	Dir string
	// Name overrides the generated directory name.
	Name  string
	Label string

	Conn    pg.Conn
	DataDir string // empty or missing: no data archive
	// ConfigFiles are archived into config.tar.gz relative to ConfigRoot.
	ConfigRoot  string
	ConfigFiles []string

	Version string
	Profile string
	Now     func() time.Time
	Logf    func(format string, args ...any)
}

func (o CreateOptions) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// Create writes a new backup and returns its path. On any failure the
// partial directory is removed, so a half-written backup is never mistaken
// for a good one.
func Create(ctx context.Context, o CreateOptions) (path string, m *Manifest, err error) {
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	ts := now().UTC()
	name := o.Name
	if name == "" {
		name = "opengtm-" + ts.Format("20060102T150405Z")
		if o.Label != "" {
			name += "-" + sanitize(o.Label)
		}
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return "", nil, err
	}
	path = filepath.Join(o.Dir, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		return "", nil, fmt.Errorf("backup: %w", err)
	}
	created := path // path is a named result and is blanked by `return "", ...`
	defer func() {
		if err != nil {
			os.RemoveAll(created)
		}
	}()

	m = &Manifest{
		Format: FormatVersion, CreatedAt: ts, OpenGTMVersion: o.Version, Profile: o.Profile,
		Label: o.Label, Database: o.Conn.DatabaseName(),
	}
	if m.PostgresVersion, err = o.Conn.ServerVersion(ctx); err != nil {
		return "", nil, fmt.Errorf("backup: connect to database: %w", err)
	}
	if m.AlembicRevisions, err = o.Conn.Revisions(ctx); err != nil {
		return "", nil, err
	}
	if m.Tables, err = o.Conn.TableCount(ctx); err != nil {
		return "", nil, err
	}

	o.logf("dumping database %s", o.Conn.Describe())
	if err = writeFile(filepath.Join(path, DumpFile), func(w io.Writer) error { return o.Conn.Dump(ctx, w) }); err != nil {
		return "", nil, fmt.Errorf("backup: pg_dump: %w", err)
	}

	roles, rerr := o.Conn.RolesSQL(ctx)
	if rerr != nil {
		// Best effort: managed services may hide pg_roles detail. The dump is
		// still complete; restoring into the same cluster does not need it.
		o.logf("warning: roles were not captured: %v", rerr)
	} else if err = os.WriteFile(filepath.Join(path, RolesFile), []byte(roles), 0o600); err != nil {
		return "", nil, err
	}

	if st, serr := os.Stat(o.DataDir); o.DataDir != "" && serr == nil && st.IsDir() {
		o.logf("archiving data directory %s", o.DataDir)
		err = writeFile(filepath.Join(path, DataFile), func(w io.Writer) error {
			var werr error
			m.Skipped, werr = tarDir(w, o.DataDir)
			return werr
		})
		if err != nil {
			return "", nil, fmt.Errorf("backup: data directory: %w", err)
		}
	}

	if len(o.ConfigFiles) > 0 {
		err = writeFile(filepath.Join(path, ConfigFile), func(w io.Writer) error {
			return tarFiles(w, o.ConfigRoot, o.ConfigFiles)
		})
		if err != nil {
			return "", nil, fmt.Errorf("backup: config: %w", err)
		}
	}

	for _, n := range []string{DumpFile, RolesFile, DataFile, ConfigFile} {
		e, ok, herr := hashFile(filepath.Join(path, n))
		if herr != nil {
			return "", nil, herr
		}
		if ok {
			e.Name = n
			m.Files = append(m.Files, e)
		}
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", nil, err
	}
	if err = os.WriteFile(filepath.Join(path, ManifestFile), append(raw, '\n'), 0o600); err != nil {
		return "", nil, err
	}
	return path, m, nil
}

// Verify reads the manifest and checks every file's size and checksum.
func Verify(path string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(path, ManifestFile))
	if err != nil {
		return nil, fmt.Errorf("backup: %w (is %s an opengtm backup directory?)", err, path)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("backup: parse manifest: %w", err)
	}
	if m.Format != FormatVersion {
		return nil, fmt.Errorf("backup: format %d is not supported by this opengtm (expects %d)", m.Format, FormatVersion)
	}
	if _, ok := m.File(DumpFile); !ok {
		return nil, errors.New("backup: manifest lists no database dump")
	}
	for _, f := range m.Files {
		if f.Name != filepath.Base(f.Name) {
			return nil, fmt.Errorf("backup: manifest entry %q is not a plain file name", f.Name)
		}
		got, ok, err := hashFile(filepath.Join(path, f.Name))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("backup: %s is listed in the manifest but missing", f.Name)
		}
		if got.Size != f.Size || got.SHA256 != f.SHA256 {
			return nil, fmt.Errorf("backup: %s does not match its checksum (corrupt or modified)", f.Name)
		}
	}
	return &m, nil
}

// RestoreOptions configures Restore.
type RestoreOptions struct {
	Conn pg.Conn
	// DataDir, when set and the backup holds data, is replaced. A non-empty
	// existing directory needs Force and is moved aside, not deleted.
	DataDir string
	// Wipe empties the target database first. Without it the database must
	// already be empty.
	Wipe bool
	// SkipRoles skips applying roles.sql.
	SkipRoles bool
	// SkipData restores the database only.
	SkipData bool
	// ConfigDir, when set, extracts config.tar.gz there. Existing files are
	// never overwritten.
	ConfigDir string
	Now       func() time.Time
	Logf      func(format string, args ...any)
}

func (o RestoreOptions) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// ErrNotEmpty means the target database already holds objects.
var ErrNotEmpty = errors.New("target database is not empty (use --wipe to replace its contents)")

// Restore verifies the backup, then restores roles, the database and the data
// directory. The returned warnings are non-fatal problems.
func Restore(ctx context.Context, path string, o RestoreOptions) (m *Manifest, warnings []string, err error) {
	m, err = Verify(path)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}

	// Stage the data directory before touching the database, so a problem
	// with it (bad archive, no space) aborts while nothing has changed yet.
	var stagedData string
	if _, ok := m.File(DataFile); ok && o.DataDir != "" && !o.SkipData {
		stagedData, err = extractToSibling(filepath.Join(path, DataFile), o.DataDir)
		if err != nil {
			return m, nil, fmt.Errorf("backup: stage data directory: %w", err)
		}
		defer func() {
			if stagedData != "" {
				os.RemoveAll(stagedData)
			}
		}()
	}

	empty, err := o.Conn.IsEmpty(ctx)
	if err != nil {
		return m, nil, fmt.Errorf("backup: inspect target database: %w", err)
	}
	if !empty {
		if !o.Wipe {
			return m, nil, ErrNotEmpty
		}
		o.logf("wiping target database %s", o.Conn.Describe())
		if err := o.Conn.Wipe(ctx); err != nil {
			return m, nil, fmt.Errorf("backup: wipe target database: %w", err)
		}
	}

	if _, ok := m.File(RolesFile); ok && !o.SkipRoles {
		script, rerr := os.ReadFile(filepath.Join(path, RolesFile))
		if rerr != nil {
			return m, nil, rerr
		}
		w, aerr := o.Conn.ApplyRoles(ctx, string(script))
		if aerr != nil {
			return m, nil, aerr
		}
		warnings = append(warnings, w...)
	}

	o.logf("restoring database into %s", o.Conn.Describe())
	f, err := os.Open(filepath.Join(path, DumpFile))
	if err != nil {
		return m, warnings, err
	}
	err = o.Conn.Restore(ctx, f)
	f.Close()
	if err != nil {
		return m, warnings, fmt.Errorf("backup: pg_restore: %w", err)
	}

	if stagedData != "" {
		old, serr := swapDir(o.DataDir, stagedData, now())
		if serr != nil {
			return m, warnings, fmt.Errorf("backup: install data directory: %w", serr)
		}
		stagedData = ""
		if old != "" {
			warnings = append(warnings, "previous data directory kept at "+old)
		}
	}

	if _, ok := m.File(ConfigFile); ok && o.ConfigDir != "" {
		if cerr := extractTar(filepath.Join(path, ConfigFile), o.ConfigDir, false); cerr != nil {
			warnings = append(warnings, "config not restored: "+cerr.Error())
		}
	}
	return m, warnings, nil
}

// List returns the backups under dir, newest first, skipping directories that
// are not backups.
func List(dir string) ([]ListEntry, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []ListEntry
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name(), ManifestFile))
		if err != nil {
			continue
		}
		var m Manifest
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		var size int64
		for _, f := range m.Files {
			size += f.Size
		}
		out = append(out, ListEntry{Path: filepath.Join(dir, e.Name()), Manifest: m, Size: size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.CreatedAt.After(out[j].Manifest.CreatedAt) })
	return out, nil
}

// ListEntry is one backup found by List.
type ListEntry struct {
	Path     string
	Manifest Manifest
	Size     int64
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func writeFile(path string, fill func(io.Writer) error) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := fill(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func hashFile(path string) (FileEntry, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return FileEntry{}, false, nil
	}
	if err != nil {
		return FileEntry{}, false, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return FileEntry{}, false, err
	}
	return FileEntry{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, true, nil
}

// tarDir archives root as a gzip tarball with paths relative to root.
// Regular files and directories are kept; everything else is reported.
func tarDir(w io.Writer, root string) (skipped []string, err error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if errors.Is(werr, fs.ErrNotExist) { // vanished while archiving a live directory
				return nil
			}
			return werr
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		info, ierr := d.Info()
		if ierr != nil {
			if errors.Is(ierr, fs.ErrNotExist) {
				return nil
			}
			return ierr
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			skipped = append(skipped, filepath.ToSlash(rel))
			return nil
		}
		hdr, herr := tar.FileInfoHeader(info, "")
		if herr != nil {
			return herr
		}
		hdr.Name = filepath.ToSlash(rel) + "/"
		if !info.IsDir() {
			hdr.Name = filepath.ToSlash(rel)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, oerr := os.Open(p)
		if oerr != nil {
			if errors.Is(oerr, fs.ErrNotExist) {
				return fmt.Errorf("%s vanished after its header was written; stop writers before backing up", rel)
			}
			return oerr
		}
		defer f.Close()
		// A growing file would make io.Copy exceed the header size; copy
		// exactly hdr.Size so the archive stays well-formed.
		if _, err := io.CopyN(tw, f, hdr.Size); err != nil {
			return fmt.Errorf("%s changed while being archived (%v); stop writers before backing up", rel, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return skipped, gz.Close()
}

// tarFiles archives the named files (relative to root) that exist.
func tarFiles(w io.Writer, root string, files []string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, rel := range files {
		p := filepath.Join(root, rel)
		info, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.CopyN(tw, f, hdr.Size)
		f.Close()
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// extractTar unpacks a gzip tarball into dir. Entries that would escape dir,
// and anything but regular files and directories, are rejected. With
// overwrite false, existing files are left alone.
func extractTar(archive, dir string, overwrite bool) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root := filepath.Clean(dir)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if strings.HasPrefix(hdr.Name, "/") || filepath.IsAbs(hdr.Name) {
			return fmt.Errorf("archive entry %q is an absolute path", hdr.Name)
		}
		target := filepath.Join(root, filepath.FromSlash(hdr.Name))
		if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
			return fmt.Errorf("archive entry %q escapes the destination", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			os.Chmod(target, hdr.FileInfo().Mode().Perm()) // best effort; keeps container-writable bits
			chown(target, hdr)
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
			if overwrite {
				flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
			}
			out, err := os.OpenFile(target, flags, hdr.FileInfo().Mode().Perm())
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
			os.Chtimes(target, hdr.ModTime, hdr.ModTime)
			chown(target, hdr)
		default:
			return fmt.Errorf("archive entry %q has unsupported type %q", hdr.Name, hdr.Typeflag)
		}
	}
}

// extractToSibling unpacks archive into a fresh directory next to dest and
// returns its path.
func extractToSibling(archive, dest string) (string, error) {
	parent := filepath.Dir(filepath.Clean(dest))
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(dest)+".restore-")
	if err != nil {
		return "", err
	}
	if err := extractTar(archive, tmp, true); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	return tmp, nil
}

// swapDir replaces dest with staged. A non-empty dest is renamed aside with a
// timestamp, never deleted; its new path is returned.
func swapDir(dest, staged string, now time.Time) (aside string, err error) {
	if st, serr := os.Stat(dest); serr == nil && st.IsDir() {
		ents, _ := os.ReadDir(dest)
		if len(ents) == 0 {
			if err := os.Remove(dest); err != nil {
				return "", err
			}
		} else {
			aside = filepath.Clean(dest) + ".pre-restore-" + now.UTC().Format("20060102T150405Z")
			if err := os.Rename(dest, aside); err != nil {
				return "", err
			}
		}
	}
	if err := os.Rename(staged, dest); err != nil {
		if aside != "" {
			os.Rename(aside, dest) // put the original back
		}
		return "", err
	}
	return aside, nil
}
