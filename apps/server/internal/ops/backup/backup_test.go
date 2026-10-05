package backup_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/ops/backup"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/optest"
	"github.com/debpalash/OpenGTM/apps/server/internal/ops/pg"
)

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return c
}

func rnd() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// seed builds a database that has the features a real restore must preserve:
// a role other than the owner, row-level security that references it, a
// sequence, a view and an alembic_version row.
func seed(t *testing.T, c pg.Conn, role string) {
	t.Helper()
	if err := c.Exec(ctx(t), `
CREATE TABLE alembic_version (version_num varchar(32) NOT NULL PRIMARY KEY);
INSERT INTO alembic_version VALUES ('abc123def456');
CREATE TABLE leads (id serial PRIMARY KEY, workspace_id text NOT NULL, email text NOT NULL);
INSERT INTO leads (workspace_id, email) VALUES ('w1','a@example.com'), ('w2','b@example.com'), ('w2','it''s@example.com');
ALTER TABLE leads ENABLE ROW LEVEL SECURITY;
ALTER TABLE leads FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON leads TO `+role+` USING (workspace_id = current_setting('app.workspace_id', true));
CREATE VIEW lead_count AS SELECT count(*) AS n FROM leads;
GRANT SELECT, INSERT ON leads TO `+role+`;
GRANT USAGE ON SCHEMA public TO `+role+`;`); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, c pg.Conn, sql string) string {
	t.Helper()
	rows, err := c.Query(ctx(t), sql)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%s: %v %v", sql, rows, err)
	}
	return rows[0][0]
}

func newRole(t *testing.T, server pg.Conn) string {
	t.Helper()
	role := "opengtm_bk_app_" + rnd()
	if err := server.Exec(ctx(t), "CREATE ROLE "+role+" NOLOGIN NOSUPERUSER NOBYPASSRLS"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Exec(context.Background(), "DROP ROLE IF EXISTS "+role) })
	return role
}

func writeTree(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"workspaces.db":          "sqlite-bytes",
		"workspaces/w1/a.json":   `{"a":1}`,
		"workspaces/w2/b.csv":    "x,y\n1,2\n",
		"workspaces/empty/.keep": "",
	}
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink must be reported, not followed or silently dropped.
	if err := os.Symlink("/etc/hostname", filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
}

func TestRoundTripIntoScratchDatabase(t *testing.T) {
	server := optest.Server(t)
	role := newRole(t, server)
	src := optest.Scratch(t, server, "bk_src")
	seed(t, src, role)

	work := t.TempDir()
	data := filepath.Join(work, "data")
	writeTree(t, data)
	if err := os.WriteFile(filepath.Join(work, ".env"), []byte("SECRET_KEY=s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	path, m, err := backup.Create(ctx(t), backup.CreateOptions{
		Dir: filepath.Join(work, "backups"), Conn: src, DataDir: data,
		ConfigRoot: work, ConfigFiles: []string{".env", "absent.yaml"},
		Version: "9.9.9", Profile: "lite", Label: "unit test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.OpenGTMVersion != "9.9.9" || m.Tables != 2 || len(m.AlembicRevisions) != 1 || m.AlembicRevisions[0] != "abc123def456" {
		t.Fatalf("manifest = %+v", m)
	}
	if len(m.Skipped) != 1 || m.Skipped[0] != "link" {
		t.Fatalf("skipped = %v", m.Skipped)
	}
	if _, err := backup.Verify(path); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if st, _ := os.Stat(filepath.Join(path, backup.DumpFile)); st.Mode().Perm() != 0o600 {
		t.Errorf("dump mode = %v, want 0600 (backups contain secrets and personal data)", st.Mode().Perm())
	}

	// Lose the role entirely, as on a brand new cluster: the policy in the
	// dump references it, so restore only works if roles.sql recreates it.
	if err := src.Exec(ctx(t), "DROP OWNED BY "+role+"; DROP ROLE "+role); err != nil {
		t.Fatal(err)
	}

	dst := optest.Scratch(t, server, "bk_dst")
	restoredData := filepath.Join(work, "restored-data")
	restoredCfg := filepath.Join(work, "restored-config")
	m2, warns, err := backup.Restore(ctx(t), path, backup.RestoreOptions{
		Conn: dst, DataDir: restoredData, ConfigDir: restoredCfg,
	})
	if err != nil {
		t.Fatalf("restore: %v (warnings %v)", err, warns)
	}
	t.Cleanup(func() { _ = server.Exec(context.Background(), "DROP OWNED BY "+role+"; DROP ROLE IF EXISTS "+role) })
	if m2.Database != m.Database {
		t.Errorf("database name = %q", m2.Database)
	}

	if got := count(t, dst, "SELECT count(*) FROM leads"); got != "3" {
		t.Errorf("rows = %s, want 3", got)
	}
	if got := count(t, dst, "SELECT email FROM leads WHERE id = 3"); got != "it's@example.com" {
		t.Errorf("quoted value = %q", got)
	}
	if got := count(t, dst, "SELECT n FROM lead_count"); got != "3" {
		t.Errorf("view = %s", got)
	}
	// The sequence continues where it left off, so new inserts do not collide.
	if got := count(t, dst, "INSERT INTO leads (workspace_id, email) VALUES ('w3','c@example.com') RETURNING id"); got != "4" {
		t.Errorf("next id = %s, want 4", got)
	}
	if got := count(t, dst, "SELECT count(*) FROM pg_policies WHERE tablename='leads' AND policyname='tenant' AND '"+role+"' = ANY(roles)"); got != "1" {
		t.Errorf("RLS policy for %s missing after restore", role)
	}
	if got := count(t, dst, "SELECT relforcerowsecurity::int FROM pg_class WHERE relname='leads'"); got != "1" {
		t.Errorf("FORCE ROW LEVEL SECURITY lost")
	}
	if revs, _ := dst.Revisions(ctx(t)); len(revs) != 1 || revs[0] != "abc123def456" {
		t.Errorf("revisions = %v", revs)
	}

	if b, err := os.ReadFile(filepath.Join(restoredData, "workspaces", "w2", "b.csv")); err != nil || string(b) != "x,y\n1,2\n" {
		t.Errorf("restored data file = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(restoredData, "workspaces", "empty", ".keep")); err != nil {
		t.Errorf("empty file lost: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(restoredData, "link")); err == nil {
		t.Error("symlink should not be restored")
	}
	if b, err := os.ReadFile(filepath.Join(restoredCfg, ".env")); err != nil || string(b) != "SECRET_KEY=s3cret\n" {
		t.Errorf("restored config = %q, %v", b, err)
	}
}

func TestRestoreRefusesNonEmptyUnlessWipeAndWipeRemovesNewerObjects(t *testing.T) {
	server := optest.Server(t)
	role := newRole(t, server)
	db := optest.Scratch(t, server, "bk_inplace")
	seed(t, db, role)
	work := t.TempDir()

	path, _, err := backup.Create(ctx(t), backup.CreateOptions{Dir: work, Conn: db, Version: "1"})
	if err != nil {
		t.Fatal(err)
	}

	// "Upgrade" after the backup: new table, changed and removed data.
	if err := db.Exec(ctx(t), `CREATE TABLE added_by_new_release (id int); DELETE FROM leads WHERE id = 1; UPDATE alembic_version SET version_num = 'newrev000000'`); err != nil {
		t.Fatal(err)
	}

	if _, _, err := backup.Restore(ctx(t), path, backup.RestoreOptions{Conn: db}); !errors.Is(err, backup.ErrNotEmpty) {
		t.Fatalf("restore into a populated database: err = %v, want ErrNotEmpty", err)
	}
	if got := count(t, db, "SELECT count(*) FROM leads"); got != "2" {
		t.Fatalf("a refused restore changed the database: %s rows", got)
	}

	if _, _, err := backup.Restore(ctx(t), path, backup.RestoreOptions{Conn: db, Wipe: true}); err != nil {
		t.Fatalf("wipe restore: %v", err)
	}
	if got := count(t, db, "SELECT count(*) FROM leads"); got != "3" {
		t.Errorf("rows after rollback restore = %s, want 3", got)
	}
	if got := count(t, db, "SELECT count(*) FROM pg_tables WHERE tablename = 'added_by_new_release'"); got != "0" {
		t.Errorf("table from the newer release survived the rollback")
	}
	if revs, _ := db.Revisions(ctx(t)); len(revs) != 1 || revs[0] != "abc123def456" {
		t.Errorf("revision after rollback = %v", revs)
	}
	// public must be back to the stock grants so the runtime role keeps working.
	if got := count(t, db, "SELECT has_schema_privilege('"+role+"', 'public', 'USAGE')::int"); got != "1" {
		t.Errorf("role lost USAGE on public")
	}
}

func TestVerifyDetectsCorruptionAndRestoreChangesNothing(t *testing.T) {
	server := optest.Server(t)
	role := newRole(t, server)
	src := optest.Scratch(t, server, "bk_corrupt_src")
	seed(t, src, role)
	work := t.TempDir()
	path, _, err := backup.Create(ctx(t), backup.CreateOptions{Dir: work, Conn: src})
	if err != nil {
		t.Fatal(err)
	}

	dump := filepath.Join(path, backup.DumpFile)
	raw, _ := os.ReadFile(dump)
	raw[len(raw)/2] ^= 0xff
	if err := os.WriteFile(dump, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Verify(path); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("Verify = %v, want checksum error", err)
	}

	dst := optest.Scratch(t, server, "bk_corrupt_dst")
	if _, _, err := backup.Restore(ctx(t), path, backup.RestoreOptions{Conn: dst, Wipe: true}); err == nil {
		t.Fatal("restore of a corrupt backup succeeded")
	}
	if empty, _ := dst.IsEmpty(ctx(t)); !empty {
		t.Error("corrupt restore modified the target database")
	}
}

func TestCreateFailureLeavesNoPartialBackup(t *testing.T) {
	server := optest.Server(t)
	missing := server.WithDatabase("opengtm_does_not_exist_" + rnd())
	dir := t.TempDir()
	if _, _, err := backup.Create(ctx(t), backup.CreateOptions{Dir: dir, Conn: missing}); err == nil {
		t.Fatal("backup of a missing database succeeded")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("partial backup left behind: %v", ents)
	}
}

func TestExistingDataDirIsMovedAsideNotDeleted(t *testing.T) {
	server := optest.Server(t)
	role := newRole(t, server)
	src := optest.Scratch(t, server, "bk_data_src")
	seed(t, src, role)
	work := t.TempDir()
	data := filepath.Join(work, "data")
	writeTree(t, data)
	path, _, err := backup.Create(ctx(t), backup.CreateOptions{Dir: filepath.Join(work, "b"), Conn: src, DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	// State written after the backup, which a rollback discards.
	if err := os.WriteFile(filepath.Join(data, "newer.txt"), []byte("later"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := optest.Scratch(t, server, "bk_data_dst")
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	_, warns, err := backup.Restore(ctx(t), path, backup.RestoreOptions{Conn: dst, DataDir: data, Now: func() time.Time { return fixed }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "newer.txt")); err == nil {
		t.Error("restored data dir still has the newer file")
	}
	aside := data + ".pre-restore-20260102T030405Z"
	if b, err := os.ReadFile(filepath.Join(aside, "newer.txt")); err != nil || string(b) != "later" {
		t.Errorf("previous data dir not kept at %s: %v", aside, err)
	}
	if len(warns) == 0 || !strings.Contains(warns[len(warns)-1], aside) {
		t.Errorf("warnings %v should name the kept directory", warns)
	}
}

func TestExtractRejectsPathTraversalAndLinks(t *testing.T) {
	build := func(h tar.Header, body string) string {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		h.Size = int64(len(body))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
		_ = tw.Close()
		_ = gz.Close()
		p := filepath.Join(t.TempDir(), "x.tar.gz")
		if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, h := range map[string]tar.Header{
		"dotdot":  {Name: "../escape.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		"abs":     {Name: "/tmp/escape-abs.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		"symlink": {Name: "l", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "dest")
			err := backup.ExtractForTest(build(h, "x"), dest)
			if err == nil {
				t.Fatal("hostile archive was extracted")
			}
			if _, serr := os.Stat(filepath.Join(parent, "escape.txt")); serr == nil {
				t.Fatal("file escaped the destination")
			}
		})
	}
}
