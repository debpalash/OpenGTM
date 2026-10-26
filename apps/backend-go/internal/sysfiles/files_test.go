package sysfiles

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tree builds <tmp>/data plus an outside canary tree and returns (data, tmp).
func tree(t *testing.T) (string, string) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(tmp, "data")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(data, "sub", "deeper"), 0o755))
	must(os.MkdirAll(filepath.Join(data, "emptydir"), 0o755))
	must(os.MkdirAll(filepath.Join(tmp, "data-evil"), 0o755))
	must(os.MkdirAll(filepath.Join(tmp, "outside"), 0o755))
	must(os.WriteFile(filepath.Join(tmp, "data-evil", "secret.txt"), []byte("evil"), 0o644))
	must(os.WriteFile(filepath.Join(tmp, "outside", "secret.txt"), []byte("outside"), 0o644))
	must(os.WriteFile(filepath.Join(tmp, "x"), []byte("x"), 0o644))
	must(os.WriteFile(filepath.Join(data, "a.txt"), []byte("a"), 0o644))
	must(os.WriteFile(filepath.Join(data, "sub", "inner.md"), []byte("inner"), 0o644))
	must(os.Symlink(filepath.Join(tmp, "outside"), filepath.Join(data, "escape")))
	must(os.Symlink(filepath.Join(tmp, "outside", "secret.txt"), filepath.Join(data, "escapefile")))
	return data, tmp
}

func touch(t *testing.T, p string, ts string) {
	t.Helper()
	tm, err := time.Parse("2006-01-02 15:04:05", ts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, tm, tm); err != nil {
		t.Fatal(err)
	}
}

func TestStatsShape(t *testing.T) {
	data, tmp := tree(t)
	sqlite := filepath.Join(tmp, "app.db")
	if err := os.WriteFile(sqlite, make([]byte, 123), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tmp, "outside"), filepath.Join(data, "loop")); err != nil {
		t.Fatal(err)
	}
	s, err := Stats(t.Context(), "/", tmp, "sqlite:///"+sqlite)
	if err != nil {
		t.Fatal(err)
	}
	if s.DiskTotal == 0 || s.DiskUsed == 0 || s.DiskFree == 0 || s.DiskUsed > s.DiskTotal {
		t.Errorf("disk numbers implausible: %+v", s)
	}
	if s.DBSize != 123 {
		t.Errorf("db_size = %d, want 123", s.DBSize)
	}
	// a.txt(1)+inner.md(5)+secret(4)+secret(7)+x(1)+app.db(123); symlinks are skipped.
	if s.ProjectSize != 1+5+4+7+1+123 {
		t.Errorf("project_size = %d", s.ProjectSize)
	}
	if s2, _ := Stats(t.Context(), "/", tmp, "postgresql+psycopg://u@h/db"); s2.DBSize != 0 {
		t.Errorf("non-sqlite db_size = %d, want 0", s2.DBSize)
	}
}

func TestListSortingAndHiddenFiles(t *testing.T) {
	data, _ := tree(t)
	for _, n := range []string{".hidden", "b.mp4", "c.PNG", "d.wav", "e.pdf", "f.zzz"} {
		if err := os.WriteFile(filepath.Join(data, n), []byte("12"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	touch(t, filepath.Join(data, "a.txt"), "2026-01-01 10:00:00")
	touch(t, filepath.Join(data, "b.mp4"), "2026-01-02 10:00:00")
	touch(t, filepath.Join(data, "c.PNG"), "2026-01-03 10:00:00")
	touch(t, filepath.Join(data, "d.wav"), "2026-01-04 10:00:00")
	touch(t, filepath.Join(data, "e.pdf"), "2026-01-05 10:00:00")
	touch(t, filepath.Join(data, "f.zzz"), "2026-01-06 10:00:00")
	touch(t, filepath.Join(data, "sub"), "2026-01-10 10:00:00")
	touch(t, filepath.Join(data, "emptydir"), "2026-01-11 10:00:00")
	t.Setenv("TZ", "UTC")
	old := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = old })

	names := func(legacy bool) []string {
		l, err := New(data, legacy).List("")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range l {
			if e.Name == ".hidden" {
				t.Errorf("hidden file listed (legacy=%v)", legacy)
			}
			out = append(out, e.Name)
		}
		return out
	}
	// Python quirk kept for parity: reverse=True on (not is_dir, modified) puts FILES first.
	// Hardened mode hides symlinks that point outside the data dir ("escape", "escapefile").
	got := strings.Join(names(false), ",")
	if want := "f.zzz,e.pdf,d.wav,c.PNG,b.mp4,a.txt,emptydir,sub"; got != want {
		t.Fatalf("hardened order = %s, want %s", got, want)
	}
	if lg := names(true); len(lg) != 10 {
		t.Errorf("legacy lists symlinks too, got %v", lg)
	}
	first, _ := New(data, false).List("")
	if e := first[0]; e.Modified != "2026-01-06T10:00:00" || e.Type != "other" || e.Size != 2 || e.Path != "f.zzz" {
		t.Errorf("entry = %+v", e)
	}
	l, _ := New(data, false).List("")
	types := map[string]string{}
	for _, e := range l {
		types[e.Name] = e.Type
	}
	for n, w := range map[string]string{"b.mp4": "video", "c.PNG": "image", "d.wav": "audio", "e.pdf": "document", "f.zzz": "other", "sub": "folder"} {
		if types[n] != w {
			t.Errorf("type(%s) = %q, want %q", n, types[n], w)
		}
	}
	sub, _ := New(data, false).List("sub")
	if len(sub) != 2 || sub[0].Path != filepath.Join("sub", "inner.md") || sub[0].Name != "inner.md" {
		t.Errorf("sub listing = %+v", sub)
	}
}

func TestListMissingDirIsEmpty(t *testing.T) {
	data, _ := tree(t)
	for _, legacy := range []bool{false, true} {
		l, err := New(data, legacy).List("nope")
		if err != nil || l == nil || len(l) != 0 {
			t.Errorf("missing dir: %v %v (legacy=%v)", l, err, legacy)
		}
		l, err = New(filepath.Join(data, "missing-root"), legacy).List("")
		if err != nil || l == nil || len(l) != 0 {
			t.Errorf("missing root: %v %v", l, err)
		}
	}
}

func TestTraversalRejected(t *testing.T) {
	data, tmp := tree(t)
	cases := []string{
		"../x", "/etc/passwd", "a/../../x", "..", "../data-evil/secret.txt", "../data-evil",
		"sub/../../data-evil/secret.txt", "escape", "escape/secret.txt", "escapefile", "a.txt\x00/../../x",
		"sub/../a.txt", // any dot-dot segment is refused outright (symlink-safe)
	}
	f := New(data, false)
	for _, c := range cases {
		if _, err := f.List(c); !errors.Is(err, ErrAccessDenied) {
			t.Errorf("List(%q) err = %v, want ErrAccessDenied", c, err)
		}
		if _, _, err := f.Open(c); !errors.Is(err, ErrAccessDenied) {
			t.Errorf("Open(%q) err = %v, want ErrAccessDenied", c, err)
		}
		// escape / escapefile are symlinks INSIDE the root: deleting the link itself is allowed and
		// never touches the target, so they are checked separately below.
		if c == "escape" || c == "escapefile" {
			continue
		}
		if err := f.Delete(c); !errors.Is(err, ErrAccessDenied) {
			t.Errorf("Delete(%q) err = %v, want ErrAccessDenied", c, err)
		}
	}
	for _, p := range []string{"x", "data-evil/secret.txt", "outside/secret.txt"} {
		if _, err := os.Stat(filepath.Join(tmp, p)); err != nil {
			t.Errorf("outside file %s was removed: %v", p, err)
		}
	}
	// Deleting a symlink removes only the link.
	if err := f.Delete("escape"); err != nil {
		t.Fatalf("delete symlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "outside", "secret.txt")); err != nil {
		t.Errorf("symlink delete followed the link: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(data, "escape")); !os.IsNotExist(err) {
		t.Errorf("link still present: %v", err)
	}
	// A normal in-root path still works.
	if p, fi, err := f.Open("sub/inner.md"); err != nil || fi.Size() != 5 || filepath.Base(p) != "inner.md" {
		t.Errorf("Open ok path: %v %v %v", p, fi, err)
	}
}

func TestSiblingPrefixRootIsNotInside(t *testing.T) {
	_, tmp := tree(t)
	// root "<tmp>/data" must not accept "<tmp>/data-evil" even in a lexical prefix sense; legacy does.
	f := New(filepath.Join(tmp, "data"), false)
	if _, _, err := f.Open("../data-evil/secret.txt"); !errors.Is(err, ErrAccessDenied) {
		t.Errorf("hardened accepted sibling prefix: %v", err)
	}
	lf := New(filepath.Join(tmp, "data"), true)
	if _, _, err := lf.Open("../data-evil/secret.txt"); err != nil {
		t.Errorf("legacy must keep the string-prefix quirk: %v", err)
	}
}

func TestDeleteRefusesRootAndNonEmptyDir(t *testing.T) {
	data, _ := tree(t)
	f := New(data, false)
	for _, root := range []string{"", ".", "./", "sub/.."} {
		err := f.Delete(root)
		if !errors.Is(err, ErrRoot) && !errors.Is(err, ErrAccessDenied) {
			t.Errorf("Delete(%q) = %v, want ErrRoot/ErrAccessDenied", root, err)
		}
	}
	if _, err := os.Stat(filepath.Join(data, "sub", "inner.md")); err != nil {
		t.Fatalf("tree damaged: %v", err)
	}
	if err := f.Delete("sub"); !errors.Is(err, ErrNotEmpty) {
		t.Errorf("Delete(non-empty) = %v, want ErrNotEmpty", err)
	}
	if _, err := os.Stat(filepath.Join(data, "sub", "inner.md")); err != nil {
		t.Errorf("non-empty dir content removed: %v", err)
	}
	if err := f.Delete("emptydir"); err != nil {
		t.Errorf("Delete(empty dir) = %v", err)
	}
	if err := f.Delete("a.txt"); err != nil {
		t.Errorf("Delete(file) = %v", err)
	}
	if err := f.Delete("a.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete(missing) = %v, want ErrNotFound", err)
	}
}

func TestLegacyDeleteIsRecursive(t *testing.T) {
	data, _ := tree(t)
	if err := New(data, true).Delete("sub"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "sub")); !os.IsNotExist(err) {
		t.Errorf("legacy rmtree did not remove sub: %v", err)
	}
}

func TestDanglingSymlinkRejected(t *testing.T) {
	data, tmp := tree(t)
	target := filepath.Join(tmp, "not-yet-created")
	if err := os.Symlink(target, filepath.Join(data, "dangling")); err != nil {
		t.Fatal(err)
	}
	f := New(data, false)
	for _, c := range []string{"dangling", "dangling/x"} {
		if _, _, err := f.Open(c); !errors.Is(err, ErrAccessDenied) {
			t.Errorf("Open(%q) = %v, want ErrAccessDenied", c, err)
		}
		if _, err := f.List(c); !errors.Is(err, ErrAccessDenied) {
			t.Errorf("List(%q) = %v, want ErrAccessDenied", c, err)
		}
	}
	if err := f.Delete("dangling/x"); !errors.Is(err, ErrAccessDenied) {
		t.Errorf("Delete(dangling/x) = %v", err)
	}
	// The outside target appearing later must not become readable through the link.
	if err := os.WriteFile(target, []byte("late"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fh, _, err := f.OpenFile("dangling"); err == nil {
		fh.Close()
		t.Fatal("OpenFile followed a symlink to a file created outside the data dir")
	}
	// Deleting the link itself is still allowed and leaves the target alone.
	if err := f.Delete("dangling"); err != nil {
		t.Errorf("Delete(dangling link) = %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("target removed: %v", err)
	}
}

func TestOpenFileNoFollowAndRecheck(t *testing.T) {
	data, _ := tree(t)
	if err := os.Symlink(filepath.Join(data, "a.txt"), filepath.Join(data, "inlink")); err != nil {
		t.Fatal(err)
	}
	f := New(data, false)
	fh, st, err := f.OpenFile("inlink") // in-root link resolves to a.txt, then opens without following
	if err != nil || st.Size() != 1 || filepath.Base(fh.Name()) != "a.txt" {
		t.Fatalf("OpenFile in-root link: %v %v", err, st)
	}
	fh.Close()
	for _, c := range []string{"escapefile", "escape/secret.txt", "../x"} {
		if fh, _, err := f.OpenFile(c); err == nil {
			fh.Close()
			t.Errorf("OpenFile(%q) succeeded", c)
		}
	}
	if _, _, err := f.OpenFile("sub"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenFile(dir) = %v", err)
	}
}

func TestListCapTruncates(t *testing.T) {
	data, _ := tree(t)
	for i := 0; i < 5; i++ {
		p := filepath.Join(data, "f"+string(rune('0'+i))+".txt")
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		touch(t, p, "2026-02-0"+string(rune('1'+i))+" 10:00:00")
	}
	l, trunc, err := New(data, false).WithListLimit(3).ListTruncated("")
	if err != nil || !trunc || len(l) != 3 || l[1].Name != "f4.txt" {
		t.Fatalf("capped list: %v %v %+v", err, trunc, l)
	}
	if l, trunc, _ := New(data, false).ListTruncated(""); trunc || len(l) < 8 {
		t.Errorf("default cap truncated a small dir: %v %d", trunc, len(l))
	}
	if l, trunc, _ := New(data, true).WithListLimit(3).ListTruncated(""); trunc || len(l) < 8 {
		t.Errorf("legacy must stay unbounded: %v %d", trunc, len(l))
	}
}

func TestStatsWalkCaps(t *testing.T) {
	data, _ := tree(t)
	s, err := Stats(t.Context(), "/", data, "", StatsLimits{MaxEntries: 3})
	if err != nil || !s.ProjectSizePartial {
		t.Fatalf("capped stats: %+v %v", s, err)
	}
	s, _ = Stats(t.Context(), "/", data, "", StatsLimits{MaxEntries: 1000, MaxTime: time.Minute})
	if s.ProjectSizePartial {
		t.Errorf("small tree reported partial")
	}
	if s, _ = Stats(t.Context(), "/", data, ""); s.ProjectSizePartial {
		t.Errorf("unbounded stats reported partial")
	}
}
