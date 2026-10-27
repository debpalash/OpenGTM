//go:build integration

package migrate_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/migrate"
)

const appRole = "yupcha_app"

// scratchDB creates an empty database on the TEST_DATABASE_URL cluster and
// returns its URL; it is dropped when the test ends.
func scratchDB(t *testing.T) string {
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
	name := "opengtm_migtest_" + hex.EncodeToString(b)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		conn.Close(context.Background())
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

func openDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func openPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func run(t *testing.T, dsn string) migrate.Result {
	t.Helper()
	res, err := migrate.Run(context.Background(), openDB(t, dsn), migrate.Options{AppRole: appRole})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return res
}

func scalar[T any](t *testing.T, p *pgxpool.Pool, q string, args ...any) T {
	t.Helper()
	var v T
	if err := p.QueryRow(context.Background(), q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func TestRunFreshAppliesBaseline(t *testing.T) {
	dsn := scratchDB(t)
	res := run(t, dsn)
	if res.Adopted || res.From != 0 || res.To != migrate.BaselineVersion || !slices.Contains(res.Applied, migrate.BaselineVersion) {
		t.Fatalf("result %+v", res)
	}
	p := openPool(t, dsn)
	if v := scalar[string](t, p, `SELECT version_num FROM alembic_version`); v != migrate.BaselineAlembicRevision {
		t.Fatalf("alembic stamp %q", v)
	}
	if n := scalar[int](t, p, `SELECT count(*) FROM pg_tables WHERE schemaname='public'`); n < 80 {
		t.Fatalf("only %d tables", n)
	}
	fn := "public.outreach_send_by_message_id(text)"
	if !scalar[bool](t, p, `SELECT has_function_privilege($1, $2, 'EXECUTE')`, appRole, fn) {
		t.Error("app role cannot execute the message lookup")
	}
	if scalar[bool](t, p, `SELECT EXISTS (SELECT 1 FROM pg_proc p, aclexplode(p.proacl) a WHERE p.oid = $1::regprocedure AND a.grantee = 0)`, fn) {
		t.Error("PUBLIC can execute the message lookup")
	}
	for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
		if !scalar[bool](t, p, `SELECT has_table_privilege($1, 'public.workbook_rows', $2)`, appRole, priv) {
			t.Errorf("app role lacks %s on workbook_rows", priv)
		}
	}
	if !scalar[bool](t, p, `SELECT has_table_privilege($1, 'goose_db_version', 'SELECT')`, appRole) {
		t.Error("app role cannot read goose_db_version (startup check)")
	}
	if err := migrate.CheckSchema(context.Background(), p, migrate.BaselineVersion); err != nil {
		t.Fatalf("CheckSchema: %v", err)
	}
}

func TestRunIsIdempotent(t *testing.T) {
	dsn := scratchDB(t)
	run(t, dsn)
	res := run(t, dsn)
	if len(res.Applied) != 0 || res.Adopted || res.From != res.To || res.To != migrate.BaselineVersion {
		t.Fatalf("second run %+v", res)
	}
	p := openPool(t, dsn)
	if n := scalar[int](t, p, `SELECT count(*) FROM goose_db_version WHERE version_id = $1`, migrate.BaselineVersion); n != 1 {
		t.Fatalf("baseline recorded %d times", n)
	}
}

// alembicOnly turns a Go-migrated database into one that looks Alembic-managed
// (same schema, alembic_version stamped, no goose table).
func alembicOnly(t *testing.T, p *pgxpool.Pool, rev string) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{`DROP TABLE goose_db_version`, `UPDATE alembic_version SET version_num = '` + rev + `'`} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunAdoptsAlembicHead(t *testing.T) {
	dsn := scratchDB(t)
	run(t, dsn)
	p := openPool(t, dsn)
	alembicOnly(t, p, migrate.BaselineAlembicRevision)
	if _, err := p.Exec(context.Background(), `INSERT INTO app_settings (key, value) VALUES ('wp_mig_marker', 'kept')`); err != nil {
		t.Fatal(err)
	}
	res := run(t, dsn)
	if !res.Adopted || res.To != migrate.BaselineVersion {
		t.Fatalf("result %+v", res)
	}
	if v := scalar[string](t, p, `SELECT value FROM app_settings WHERE key = 'wp_mig_marker'`); v != "kept" {
		t.Fatalf("marker %q", v)
	}
	if v := scalar[string](t, p, `SELECT version_num FROM alembic_version`); v != migrate.BaselineAlembicRevision {
		t.Fatalf("alembic stamp %q", v)
	}
	if !scalar[bool](t, p, `SELECT has_table_privilege($1, 'goose_db_version', 'SELECT')`, appRole) {
		t.Error("app role cannot read goose_db_version after adoption")
	}
	if err := migrate.CheckSchema(context.Background(), p, migrate.BaselineVersion); err != nil {
		t.Fatal(err)
	}
}

func TestRunRefusesOtherAlembicRevision(t *testing.T) {
	dsn := scratchDB(t)
	run(t, dsn)
	p := openPool(t, dsn)
	alembicOnly(t, p, "d3f5a7b9c1e2")
	_, err := migrate.Run(context.Background(), openDB(t, dsn), migrate.Options{AppRole: appRole})
	if err == nil || !strings.Contains(err.Error(), "d3f5a7b9c1e2") || !strings.Contains(err.Error(), "alembic upgrade") {
		t.Fatalf("err = %v", err)
	}
	if scalar[bool](t, p, `SELECT to_regclass('goose_db_version') IS NOT NULL`) {
		t.Error("refused run created goose_db_version")
	}
}

func TestRunRefusesUnmanagedSchema(t *testing.T) {
	dsn := scratchDB(t)
	p := openPool(t, dsn)
	if _, err := p.Exec(context.Background(), `CREATE TABLE stray (id int)`); err != nil {
		t.Fatal(err)
	}
	_, err := migrate.Run(context.Background(), openDB(t, dsn), migrate.Options{AppRole: appRole})
	if err == nil || !strings.Contains(err.Error(), "not managed") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunConcurrent(t *testing.T) {
	dsn := scratchDB(t)
	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	applied := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := sql.Open("pgx", dsn)
			if err != nil {
				errs[i] = err
				return
			}
			defer db.Close()
			res, err := migrate.Run(context.Background(), db, migrate.Options{AppRole: appRole})
			errs[i], applied[i] = err, len(res.Applied)
		}()
	}
	wg.Wait()
	total := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("run %d: %v", i, errs[i])
		}
		total += applied[i]
	}
	if total != 1 {
		t.Fatalf("baseline applied %d times across concurrent runs", total)
	}
	p := openPool(t, dsn)
	if c := scalar[int](t, p, `SELECT count(*) FROM goose_db_version WHERE version_id = $1`, migrate.BaselineVersion); c != 1 {
		t.Fatalf("baseline recorded %d times", c)
	}
}

func TestCheckSchema(t *testing.T) {
	dsn := scratchDB(t)
	p := openPool(t, dsn)
	ctx := context.Background()
	err := migrate.CheckSchema(ctx, p, migrate.BaselineVersion)
	if err == nil || !strings.Contains(err.Error(), "opengtm-migrate") {
		t.Fatalf("unmigrated: err = %v", err)
	}
	run(t, dsn)
	if err := migrate.CheckSchema(ctx, p, migrate.BaselineVersion); err != nil {
		t.Fatalf("migrated: %v", err)
	}
	err = migrate.CheckSchema(ctx, p, migrate.BaselineVersion+1)
	if err == nil || !strings.Contains(err.Error(), "below required") {
		t.Fatalf("behind: err = %v", err)
	}
}

func TestProvisionRuntimeLogin(t *testing.T) {
	dsn := scratchDB(t)
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	login := "wpmig_rt_" + hex.EncodeToString(b)
	opts := migrate.Options{AppRole: appRole, RuntimeLogin: &migrate.RuntimeLogin{User: login, Password: "pw-" + hex.EncodeToString(b)}}
	ctx := context.Background()
	for range 2 { // idempotent: second run takes the ALTER ROLE path
		if _, err := migrate.Run(ctx, openDB(t, dsn), opts); err != nil {
			t.Fatal(err)
		}
	}
	p := openPool(t, dsn)
	t.Cleanup(func() {
		// The scratch database (CONNECT, schema USAGE) is dropped first by its own cleanup.
		admin, err := pgx.Connect(context.Background(), os.Getenv("TEST_DATABASE_URL"))
		if err == nil {
			_, _ = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+login)
			admin.Close(context.Background())
		}
	})
	if !scalar[bool](t, p, `SELECT rolcanlogin AND NOT rolsuper AND NOT rolbypassrls FROM pg_roles WHERE rolname = $1`, login) {
		t.Error("login role flags")
	}
	if !scalar[bool](t, p, `SELECT pg_has_role($1, $2, 'MEMBER')`, login, appRole) {
		t.Error("login is not a member of the app role")
	}
	if !scalar[bool](t, p, `SELECT has_database_privilege($1, current_database(), 'CONNECT')`, login) {
		t.Error("login lacks CONNECT")
	}
	u, _ := url.Parse(dsn)
	u.User = url.UserPassword(login, opts.RuntimeLogin.Password)
	rt := openPool(t, u.String())
	if err := migrate.CheckSchema(ctx, rt, migrate.BaselineVersion); err != nil {
		t.Fatalf("runtime login startup check: %v", err)
	}
}

func TestProvisionRequiresPassword(t *testing.T) {
	dsn := scratchDB(t)
	_, err := migrate.Run(context.Background(), openDB(t, dsn), migrate.Options{AppRole: appRole, RuntimeLogin: &migrate.RuntimeLogin{User: "wpmig_nopw"}})
	if err == nil || !strings.Contains(err.Error(), "YUPCHA_RUNTIME_DB_PASSWORD") {
		t.Fatalf("err = %v", err)
	}
}

func schemaSQLPath(t *testing.T) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "db", "schema.sql")
}

// TestSchemaSQLMatchesMigrations is the CI parity check: internal/db/schema.sql
// (the sqlc input) must describe exactly the schema the Go migrations build.
// Regenerate with `make schema` (scripts/go_schema_dump.sh) when it fails.
func TestSchemaSQLMatchesMigrations(t *testing.T) {
	fromMigrations := scratchDB(t)
	run(t, fromMigrations)
	fromFile := scratchDB(t)
	body, err := os.ReadFile(schemaSQLPath(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, fromFile)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SET check_function_bodies = off"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, string(body)); err != nil {
		t.Fatalf("load schema.sql: %v", err)
	}
	diffFingerprints(t, "schema.sql", fingerprint(t, openPool(t, fromFile), ""), "migrations", fingerprint(t, openPool(t, fromMigrations), ""))
}

// TestBaselineMatchesAlembicDatabase compares a Go-migrated database with a real
// Alembic-migrated one at the baseline revision, grants of the app role included.
// Opt-in: MIGRATE_ALEMBIC_DATABASE_URL (read only).
func TestBaselineMatchesAlembicDatabase(t *testing.T) {
	alembicURL := os.Getenv("MIGRATE_ALEMBIC_DATABASE_URL")
	if alembicURL == "" {
		t.Skip("MIGRATE_ALEMBIC_DATABASE_URL not set")
	}
	dsn := scratchDB(t)
	run(t, dsn)
	alembic := openPool(t, alembicURL)
	if v := scalar[string](t, alembic, `SELECT version_num FROM alembic_version`); v != migrate.BaselineAlembicRevision {
		t.Fatalf("alembic database at %q", v)
	}
	diffFingerprints(t, "alembic", fingerprint(t, alembic, appRole), "go", fingerprint(t, openPool(t, dsn), appRole))
}

func diffFingerprints(t *testing.T, an string, a []string, bn string, b []string) {
	t.Helper()
	onlyA, onlyB := minus(a, b), minus(b, a)
	for _, s := range onlyA[:min(len(onlyA), 20)] {
		t.Errorf("only in %s: %s", an, s)
	}
	for _, s := range onlyB[:min(len(onlyB), 20)] {
		t.Errorf("only in %s: %s", bn, s)
	}
	if len(a) < 500 {
		t.Errorf("fingerprint suspiciously small: %d entries", len(a))
	}
}

func minus(a, b []string) []string {
	set := make(map[string]int, len(b))
	for _, s := range b {
		set[s]++
	}
	var out []string
	for _, s := range a {
		if set[s] > 0 {
			set[s]--
			continue
		}
		out = append(out, s)
	}
	return out
}

// fingerprint describes the public schema through the catalogs (no pg_dump
// needed, so it runs in CI). role != "" adds that role's privileges and the
// PUBLIC grants on functions.
func fingerprint(t *testing.T, p *pgxpool.Pool, role string) []string {
	t.Helper()
	const pub = `n.nspname = 'public'`
	const notExt = `NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = %s AND d.deptype = 'e')`
	const skip = `'goose_db_version', 'goose_db_version_id_seq'`
	queries := map[string]string{
		"rel": `SELECT c.relname || '|' || c.relkind::text || '|' || c.relrowsecurity || '|' || c.relforcerowsecurity || '|' || c.relpersistence::text
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE ` + pub + ` AND c.relkind IN ('r','p','v','m','S','f') AND c.relname NOT IN (` + skip + `) AND ` + fmt.Sprintf(notExt, "c.oid"),
		"col": `SELECT c.relname || '.' || a.attname || '|' || row_number() OVER (PARTITION BY c.oid ORDER BY a.attnum) || '|' ||
				format_type(a.atttypid, a.atttypmod) || '|' || a.attnotnull || '|' || coalesce(pg_get_expr(ad.adbin, ad.adrelid), '') || '|' ||
				a.attidentity::text || '|' || a.attgenerated::text || '|' || coalesce(co.collname, '')
			FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
			LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
			LEFT JOIN pg_collation co ON co.oid = a.attcollation AND a.attcollation <> 0 AND co.collname <> 'default'
			WHERE ` + pub + ` AND a.attnum > 0 AND NOT a.attisdropped AND c.relkind IN ('r','p','v','m','f') AND c.relname NOT IN (` + skip + `)`,
		"con": `SELECT c.relname || '|' || k.conname || '|' || k.contype::text || '|' || pg_get_constraintdef(k.oid)
			FROM pg_constraint k JOIN pg_class c ON c.oid = k.conrelid JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE ` + pub + ` AND c.relname NOT IN (` + skip + `)`,
		"idx": `SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND tablename NOT IN (` + skip + `)`,
		"fn": `SELECT p.proname || '|' || pg_get_function_identity_arguments(p.oid) || '|' || pg_get_functiondef(p.oid)
			FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE ` + pub + ` AND p.prokind IN ('f','p') AND ` + fmt.Sprintf(notExt, "p.oid"),
		"trg": `SELECT c.relname || '|' || pg_get_triggerdef(tg.oid) FROM pg_trigger tg JOIN pg_class c ON c.oid = tg.tgrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace WHERE ` + pub + ` AND NOT tg.tgisinternal`,
		"pol": `SELECT tablename || '|' || policyname || '|' || permissive || '|' || roles::text || '|' || cmd || '|' ||
				coalesce(qual, '') || '|' || coalesce(with_check, '') FROM pg_policies WHERE schemaname = 'public'`,
		"view": `SELECT c.relname || '|' || pg_get_viewdef(c.oid) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE ` + pub + ` AND c.relkind IN ('v','m')`,
		"seq": `SELECT sequencename || '|' || data_type || '|' || start_value || '|' || increment_by || '|' || min_value || '|' ||
				max_value || '|' || cycle || '|' || cache_size FROM pg_sequences WHERE schemaname = 'public' AND sequencename NOT IN (` + skip + `)`,
		"seqown": `SELECT s.relname || '|' || coalesce(t.relname || '.' || a.attname, '') FROM pg_class s
			JOIN pg_namespace n ON n.oid = s.relnamespace
			LEFT JOIN pg_depend d ON d.objid = s.oid AND d.classid = 'pg_class'::regclass AND d.refclassid = 'pg_class'::regclass AND d.deptype IN ('a','i')
			LEFT JOIN pg_class t ON t.oid = d.refobjid LEFT JOIN pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
			WHERE ` + pub + ` AND s.relkind = 'S' AND s.relname NOT IN (` + skip + `)`,
		"type": `SELECT t.typname || '|' || t.typtype::text || '|' || coalesce((SELECT string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder) FROM pg_enum e WHERE e.enumtypid = t.oid), '') ||
				'|' || coalesce(format_type(t.typbasetype, t.typtypmod), '')
			FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
			WHERE ` + pub + ` AND (t.typtype IN ('e','d') OR (t.typtype = 'c' AND (SELECT relkind FROM pg_class WHERE oid = t.typrelid) = 'c')) AND ` + fmt.Sprintf(notExt, "t.oid"),
		"ext": `SELECT extname FROM pg_extension WHERE extname <> 'plpgsql'`,
		"comment": `SELECT c.relname || '|' || coalesce(a.attname, '') || '|' || d.description FROM pg_description d
			JOIN pg_class c ON c.oid = d.objoid AND d.classoid = 'pg_class'::regclass JOIN pg_namespace n ON n.oid = c.relnamespace
			LEFT JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = d.objsubid AND d.objsubid > 0 WHERE ` + pub,
	}
	if role != "" {
		r := `(SELECT oid FROM pg_roles WHERE rolname = '` + role + `')`
		queries["acl_rel"] = `SELECT c.relname || '|' || x.privilege_type FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace,
			aclexplode(c.relacl) x WHERE ` + pub + ` AND x.grantee = ` + r + ` AND c.relname NOT IN (` + skip + `)`
		queries["acl_fn"] = `SELECT p.proname || '|' || x.grantee::regrole::text || '|' || x.privilege_type FROM pg_proc p
			JOIN pg_namespace n ON n.oid = p.pronamespace, aclexplode(coalesce(p.proacl, acldefault('f', p.proowner))) x
			WHERE ` + pub + ` AND (x.grantee = 0 OR x.grantee = ` + r + `) AND ` + fmt.Sprintf(notExt, "p.oid")
		queries["acl_schema"] = `SELECT x.privilege_type FROM pg_namespace n, aclexplode(n.nspacl) x WHERE ` + pub + ` AND x.grantee = ` + r
		queries["acl_default"] = `SELECT d.defaclobjtype::text || '|' || x.privilege_type FROM pg_default_acl d JOIN pg_namespace n ON n.oid = d.defaclnamespace,
			aclexplode(d.defaclacl) x WHERE ` + pub + ` AND x.grantee = ` + r
	}
	var out []string
	for kind, q := range queries {
		rows, err := p.Query(context.Background(), q)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		vals, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		for _, v := range vals {
			out = append(out, kind+": "+v)
		}
	}
	slices.Sort(out)
	return out
}
