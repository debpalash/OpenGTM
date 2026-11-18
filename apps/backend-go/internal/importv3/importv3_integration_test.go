//go:build integration

package importv3_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/importv3"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/migrate"
)

// scratchPG creates a migrated database on the TEST_DATABASE_URL cluster (dropped at the end).
func scratchPG(t *testing.T) *sql.DB {
	t.Helper()
	admin := os.Getenv("TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "opengtm_imp3_" + hex.EncodeToString(b)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		conn.Close(context.Background())
	})
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := migrate.Run(ctx, db, migrate.Options{AppRole: "yupcha_app"}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (username, hashed_password, is_active, is_admin, role) VALUES ('admin', 'x', true, true, 'admin')`,
		`INSERT INTO users (username, hashed_password, is_active, is_admin, role) VALUES ('second', 'x', true, false, 'user')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// dataDir builds a v3.0.0 data/ directory from the committed SQL fixtures (each *.db.sql becomes a SQLite file).
func dataDir(t *testing.T, skip ...string) string {
	t.Helper()
	dir := t.TempDir()
	files, err := filepath.Glob("testdata/v3data/*.db.sql")
	if err != nil || len(files) == 0 {
		t.Fatal("no fixtures", err)
	}
next:
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".sql")
		for _, s := range skip {
			if s == name {
				continue next
			}
		}
		script, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lite, err := sql.Open("sqlite", filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lite.Exec(string(script)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		lite.Close()
	}
	return dir
}

func count(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// inWorkspace runs a query under the workspace's RLS scope, as the API does.
func inWorkspace[T any](t *testing.T, db *sql.DB, ws, q string, args ...any) T {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// The test connects as a superuser, which bypasses RLS; drop to the runtime role so the policies apply.
	if _, err := tx.Exec(`SET LOCAL ROLE yupcha_app`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`SELECT set_config('app.workspace_id', $1, true)`, ws); err != nil {
		t.Fatal(err)
	}
	var v T
	if err := tx.QueryRow(q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func table(rep importv3.Report, name string) importv3.TableReport {
	for _, tr := range rep.Tables {
		if tr.Table == name {
			return tr
		}
	}
	return importv3.TableReport{Table: "(missing) " + name}
}

func TestImportCopiesEveryStoreVerbatimAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := scratchPG(t)
	dir := dataDir(t)
	rep, err := importv3.Run(ctx, db, importv3.Options{DataDir: dir, SkipUnmigrated: true})
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		table                     string
		source, skipped, inserted int
	}{
		{"workspaces", 2, 0, 2}, {"workspace_members", 4, 0, 4}, {"user_active_workspace", 2, 0, 2},
		{"workspace_settings", 3, 0, 3}, {"workspace_member_permissions", 1, 0, 1}, {"workspace_oidc_identities", 1, 0, 1},
		{"workspace_scim_tokens", 1, 0, 1}, {"workspace_scim_users", 1, 0, 1}, {"workspace_scim_groups", 1, 0, 1},
		{"workspace_scim_group_members", 1, 0, 1},
		{"app_settings", 3, 0, 3}, // the __stores_backend marker is never copied
		{"chat_conversations", 2, 1, 2}, {"chat_messages", 3, 1, 3}, {"chat_tool_approvals", 1, 0, 1},
		{"workbook_functions", 2, 0, 2}, {"chat_memories", 2, 0, 2},
	} {
		tr := table(rep, c.table)
		if tr.Source != c.source || tr.Skipped != c.skipped || tr.Inserted != c.inserted || tr.Present != c.source {
			t.Errorf("%s: %+v, want source=%d skipped=%d inserted=%d present=%d", c.table, tr, c.source, c.skipped, c.inserted, c.source)
		}
	}

	// Ciphertext is copied verbatim; the workspace-secret row is visible only inside its workspace.
	if v := inWorkspace[string](t, db, "ws-main", `SELECT value FROM workspace_settings WHERE key = 'hubspot_token'`); v != "enc:v1:gAAAAABmRehearsalCiphertext==" {
		t.Errorf("workspace secret changed: %q", v)
	}
	if n := inWorkspace[int](t, db, "ws-acme", `SELECT count(*) FROM workspace_settings WHERE key = 'hubspot_token'`); n != 0 {
		t.Errorf("RLS leak: ws-acme sees ws-main's secret")
	}
	var key string
	if err := db.QueryRow(`SELECT value FROM app_settings WHERE key = 'ANTHROPIC_API_KEY'`).Scan(&key); err != nil || key != "enc:v1:gAAAAABmProviderKeyCiphertext==" {
		t.Errorf("provider key %q %v", key, err)
	}
	if count(t, db, `SELECT count(*) FROM app_settings WHERE key = '__stores_backend'`) != 0 {
		t.Error("cutover marker copied")
	}
	// NULL coercions and types.
	if got := count(t, db, `SELECT count(*) FROM workspaces WHERE slug = 'acme' AND description = '' AND icon <> ''`); got != 1 {
		t.Errorf("workspace defaults not applied")
	}
	if got := count(t, db, `SELECT count(*) FROM workspace_members WHERE user_id = 2 AND role = 'member'`); got != 1 {
		t.Errorf("member role default not applied")
	}
	if got := inWorkspace[bool](t, db, "ws-acme", `SELECT active FROM workspace_scim_users WHERE user_id = 2`); got {
		t.Error("scim active should be false")
	}
	// Chat: untenanted conversation and its message are not imported; same-second messages keep their order.
	if count(t, db, `SELECT count(*) FROM chat_messages WHERE id = 'm-orphan'`) != 0 {
		t.Error("private message of an untenanted conversation imported")
	}
	var m1, m2 time.Time
	if err := db.QueryRow(`SELECT (SELECT created_at FROM chat_messages WHERE id = 'm1'), (SELECT created_at FROM chat_messages WHERE id = 'm2')`).Scan(&m1, &m2); err != nil || !m2.After(m1) {
		t.Errorf("message order not preserved: %v %v %v", m1, m2, err)
	}
	// JSON and sequence.
	if n := count(t, db, `SELECT jsonb_array_length(columns_chain) FROM workbook_functions WHERE id = 'f1'`); n != 1 {
		t.Errorf("columns_chain = %d", n)
	}
	if n := count(t, db, `SELECT jsonb_array_length(columns_chain) FROM workbook_functions WHERE id = 'f2'`); n != 0 {
		t.Errorf("broken JSON not replaced by the default")
	}
	if table(rep, "workbook_functions").BadJSON != 1 {
		t.Errorf("bad JSON not reported")
	}
	if _, err := db.Exec(`INSERT INTO chat_memories (user_id, text, created_at) VALUES ('1', 'next', now())`); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT max(id)::int FROM chat_memories`); n != 8 {
		t.Errorf("memory sequence not advanced past the imported ids: next id %d", n)
	}
	// Orphan user ids are reported, not fatal.
	if o := table(rep, "workspace_members").Orphans; len(o) != 1 || o[0] != 99 {
		t.Errorf("orphans %v", o)
	}
	// Nothing silently skipped: unimported SQLite tables are listed with their row counts.
	got := map[string]int{}
	for _, n := range rep.NotMigrated {
		got[n.File+"/"+n.Table] = n.Rows
	}
	if got["leads.db/leads"] != 2 || got["enrichment_cache.db/enrichment_cache"] != 1 || len(got) != 2 {
		t.Errorf("not migrated = %v", got)
	}

	// Re-run: nothing inserted, and a row changed in PostgreSQL since is not reverted.
	if _, err := db.Exec(`UPDATE app_settings SET value = 'changed-in-go' WHERE key = 'ANTHROPIC_API_KEY'`); err != nil {
		t.Fatal(err)
	}
	rep2, err := importv3.Run(ctx, db, importv3.Options{DataDir: dir, SkipUnmigrated: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range rep2.Tables {
		if tr.Inserted != 0 || tr.Existing != tr.Source || tr.Present != tr.Source {
			t.Errorf("re-run %s: %+v", tr.Table, tr)
		}
	}
	if err := db.QueryRow(`SELECT value FROM app_settings WHERE key = 'ANTHROPIC_API_KEY'`).Scan(&key); err != nil || key != "changed-in-go" {
		t.Errorf("a re-run reverted a row changed in the Go app: %q", key)
	}
}

func TestImportDryRunWritesNothing(t *testing.T) {
	db := scratchPG(t)
	before := count(t, db, `SELECT count(*) FROM app_settings`) // migration 3 writes one marker row
	rep, err := importv3.Run(context.Background(), db, importv3.Options{DataDir: dataDir(t), DryRun: true, SkipUnmigrated: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.DryRun || table(rep, "workspaces").Inserted != 2 || table(rep, "workspaces").Present != 2 {
		t.Errorf("dry run report: %+v", table(rep, "workspaces"))
	}
	for _, tb := range []string{"workspaces", "chat_memories", "workbook_functions"} {
		if n := count(t, db, `SELECT count(*) FROM `+tb); n != 0 {
			t.Errorf("dry run left %d rows in %s", n, tb)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM app_settings`); n != before {
		t.Errorf("dry run changed app_settings: %d -> %d", before, n)
	}
}

func TestImportRefusesOrphanedMembersAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	db := scratchPG(t)
	dir := dataDir(t)
	lite, err := sql.Open("sqlite", filepath.Join(dir, "workspaces.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lite.Exec(`INSERT INTO workspace_members VALUES ('ws-gone', 1, 'owner', 1.0)`); err != nil {
		t.Fatal(err)
	}
	lite.Close()
	_, err = importv3.Run(ctx, db, importv3.Options{DataDir: dir, SkipUnmigrated: true})
	if err == nil || !strings.Contains(err.Error(), "ws-gone") || !strings.Contains(err.Error(), "Nothing was written") {
		t.Fatalf("err = %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM workspaces`); n != 0 {
		t.Errorf("%d workspaces written despite the failure", n)
	}
	// A later, good store is still not attempted: the run stops at the first failing store.
	if n := count(t, db, `SELECT count(*) FROM app_settings WHERE key = 'ANTHROPIC_API_KEY'`); n != 0 {
		t.Errorf("later stores ran after a failure")
	}
}

func TestImportNeedsWorkspacesDBUnlessAllowed(t *testing.T) {
	ctx := context.Background()
	db := scratchPG(t)
	dir := dataDir(t, "workspaces.db")
	_, err := importv3.Run(ctx, db, importv3.Options{DataDir: dir})
	if err == nil || !strings.Contains(err.Error(), "workspaces.db not found") {
		t.Fatalf("err = %v", err)
	}
	rep, err := importv3.Run(ctx, db, importv3.Options{DataDir: dir, AllowMissingWorkspaces: true, SkipUnmigrated: true})
	if err != nil {
		t.Fatal(err)
	}
	if table(rep, "app_settings").Inserted != 3 {
		t.Errorf("app_settings %+v", table(rep, "app_settings"))
	}
	if _, err := importv3.Run(ctx, db, importv3.Options{DataDir: filepath.Join(dir, "nope")}); err == nil {
		t.Error("expected an error for a missing data dir")
	}
}

func TestImportNeverModifiesTheSourceFiles(t *testing.T) {
	db := scratchPG(t)
	dir := dataDir(t)
	before := map[string][]byte{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		before[e.Name()] = b
	}
	if _, err := importv3.Run(context.Background(), db, importv3.Options{DataDir: dir, SkipUnmigrated: true}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadDir(dir)
	if len(after) != len(entries) {
		t.Errorf("files appeared in the data dir: %d -> %d", len(entries), len(after))
	}
	for name, b := range before {
		got, _ := os.ReadFile(filepath.Join(dir, name))
		if !bytes.Equal(got, b) {
			t.Errorf("%s was modified", name)
		}
	}
}

// A data dir whose leads.db holds rows is refused before anything is written, unless SkipUnmigrated.
func TestImportStopsOnNonEmptyLeadsDBBeforeWritingAnything(t *testing.T) {
	ctx := context.Background()
	db := scratchPG(t)
	var before int
	if err := db.QueryRow(`SELECT count(*) FROM workspaces`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	rep, err := importv3.Run(ctx, db, importv3.Options{DataDir: dataDir(t)})
	if !errors.Is(err, importv3.ErrUnmigratedData) {
		t.Fatalf("err = %v, want ErrUnmigratedData", err)
	}
	for _, want := range []string{"migrate_sqlite_to_pg.py", "--skip-unmigrated", "Nothing was imported"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q: %v", want, err)
		}
	}
	if len(rep.Tables) != 0 || len(rep.NotMigrated) == 0 {
		t.Errorf("report: %d tables, %d not migrated", len(rep.Tables), len(rep.NotMigrated))
	}
	var after int
	if err := db.QueryRow(`SELECT count(*) FROM workspaces`).Scan(&after); err != nil || after != before {
		t.Errorf("workspaces %d -> %d (err %v): the refusal must write nothing", before, after, err)
	}
}

// A leads.db with no rows (v3.0.0 creates the file empty) and the enrichment cache do not block the import.
func TestImportIgnoresEmptyLeadsDBAndCaches(t *testing.T) {
	db := scratchPG(t)
	dir := dataDir(t, "leads.db")
	lite, err := sql.Open("sqlite", filepath.Join(dir, "leads.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lite.Exec(`CREATE TABLE leads (id INTEGER PRIMARY KEY, company TEXT)`); err != nil {
		t.Fatal(err)
	}
	lite.Close()
	if _, err := importv3.Run(context.Background(), db, importv3.Options{DataDir: dir}); err != nil {
		t.Fatalf("an empty leads.db plus the enrichment cache must not block: %v", err)
	}
}
