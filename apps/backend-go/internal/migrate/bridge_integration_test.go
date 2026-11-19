//go:build integration

package migrate_test

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/migrate"
)

// loadV3Schema creates the schema of a v3.0.0 database (testdata/v3.0.0-head-schema.sql, a schema-only pg_dump of
// a database the v3.0.0 tag's own Alembic built) and stamps it 3f4051627384.
func loadV3Schema(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	ddl, err := os.ReadFile("testdata/v3.0.0-head-schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// The dump's grants name the application role; Alembic had created it.
	if _, err := p.Exec(ctx, `DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'yupcha_app') THEN CREATE ROLE yupcha_app NOLOGIN; END IF; END $$`); err != nil {
		t.Fatal(err)
	}
	// The dump names the owner Alembic ran as (the test connects as whoever it is) and empties search_path for its
	// session (pooled connections must keep theirs).
	sqlText := strings.ReplaceAll(string(ddl), "FOR ROLE yupcha ", "")
	sqlText = strings.ReplaceAll(sqlText, "SELECT pg_catalog.set_config('search_path', '', false);", "")
	if _, err := p.Exec(ctx, sqlText); err != nil {
		t.Fatalf("load v3.0.0 schema: %v", err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO alembic_version (version_num) VALUES ($1)`, migrate.BridgeFromRevision); err != nil {
		t.Fatal(err)
	}
}

// schemaFingerprint is a deterministic description of everything the bridge must reproduce: columns, defaults,
// constraints, indexes, row security flags and policies, table and function privileges, and function bodies.
func schemaFingerprint(t *testing.T, p *pgxpool.Pool) []string {
	t.Helper()
	const q = `
SELECT 'col '||table_name||'.'||column_name||' '||data_type||' '||udt_name||' null='||is_nullable||' def='||coalesce(column_default,'')||' len='||coalesce(character_maximum_length::text,'')||' prec='||coalesce(numeric_precision::text,'')
  FROM information_schema.columns WHERE table_schema='public'
UNION ALL SELECT 'con '||conrelid::regclass||' '||conname||' '||pg_get_constraintdef(oid) FROM pg_constraint WHERE connamespace='public'::regnamespace
UNION ALL SELECT 'idx '||indexdef FROM pg_indexes WHERE schemaname='public'
UNION ALL SELECT 'rls '||c.relname||' '||c.relrowsecurity||' '||c.relforcerowsecurity FROM pg_class c WHERE c.relnamespace='public'::regnamespace AND c.relkind='r'
UNION ALL SELECT 'pol '||tablename||' '||policyname||' '||cmd||' '||coalesce(qual,'')||' '||coalesce(with_check,'') FROM pg_policies WHERE schemaname='public'
UNION ALL SELECT 'grant '||table_name||' '||grantee||' '||privilege_type FROM information_schema.role_table_grants WHERE table_schema='public' AND grantee NOT IN (SELECT rolname FROM pg_roles WHERE rolsuper)
UNION ALL SELECT 'fn '||p.oid::regprocedure||' sec='||p.prosecdef||' acl='||coalesce(p.proacl::text,'')||' '||md5(p.prosrc) FROM pg_proc p WHERE p.pronamespace='public'::regnamespace
UNION ALL SELECT 'seq '||sequencename||' '||data_type::text FROM pg_sequences WHERE schemaname='public'
UNION ALL SELECT 'trg '||tgrelid::regclass||' '||tgname FROM pg_trigger WHERE NOT tgisinternal
ORDER BY 1`
	rows, err := p.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBridgeUpgradesV3DatabaseToTheBaselineSchema(t *testing.T) {
	ctx := context.Background()
	dsn := scratchDB(t)
	p := openPool(t, dsn)
	loadV3Schema(t, p)

	// v3.0.0 rows: a user, a lead, queue rows in every state (the v3 worker is stopped).
	for _, q := range []string{
		`INSERT INTO users (username, hashed_password, is_active, is_admin, role) VALUES ('admin', 'x', true, true, 'admin')`,
		`INSERT INTO leads (workspace_id, company) VALUES ('ws-v3', 'Acme')`,
		`INSERT INTO jobs (type, payload, status, workspace_id) VALUES ('run_workbook', '{"workspace_id":"ws-v3"}', 'pending', 'ws-v3')`,
		`INSERT INTO jobs (type, payload, status, workspace_id, worker_id) VALUES ('collect', '{}', 'processing', 'ws-v3', 'w1')`,
		`INSERT INTO jobs (type, payload, status) VALUES ('send', '{"workspace_id":"ws-v3"}', 'pending')`,
		`INSERT INTO jobs (type, payload, status) VALUES ('send', '{}', 'pending')`,
		`INSERT INTO jobs (type, payload, status, workspace_id) VALUES ('collect', '{}', 'completed', 'ws-v3')`,
	} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	res := run(t, dsn)
	if !res.Bridged || !res.Adopted || res.To != latest(t) {
		t.Fatalf("result %+v", res)
	}
	if v := scalar[string](t, p, `SELECT version_num FROM alembic_version`); v != migrate.BaselineAlembicRevision {
		t.Fatalf("alembic stamp %q", v)
	}
	if err := migrate.CheckSchema(ctx, p, migrate.BaselineVersion); err != nil {
		t.Fatal(err)
	}

	// Core acceptance: the bridged schema equals the schema a fresh baseline run produces (which is the real Alembic
	// chain's e7b2c4d6f8a0, dumped), then the same later migrations.
	freshDSN := scratchDB(t)
	run(t, freshDSN)
	got, want := schemaFingerprint(t, p), schemaFingerprint(t, openPool(t, freshDSN))
	if !slices.Equal(got, want) {
		var diff []string
		for _, l := range got {
			if !slices.Contains(want, l) {
				diff = append(diff, "bridged only: "+l)
			}
		}
		for _, l := range want {
			if !slices.Contains(got, l) {
				diff = append(diff, "baseline only: "+l)
			}
		}
		t.Fatalf("bridged schema differs from the baseline schema (%d vs %d facts):\n%s", len(got), len(want), strings.Join(diff, "\n"))
	}

	// Data survives, and the queue is the Go worker's.
	if n := scalar[int](t, p, `SELECT count(*) FROM users`); n != 1 {
		t.Errorf("users = %d", n)
	}
	if n := scalar[int](t, p, `SELECT count(*) FROM leads WHERE workspace_id = 'ws-v3'`); n != 1 {
		t.Errorf("leads = %d", n)
	}
	if n := scalar[int](t, p, `SELECT count(*) FROM jobs`); n != 5 {
		t.Errorf("jobs = %d, want all 5 rows kept", n)
	}
	if n := scalar[int](t, p, `SELECT count(*) FROM jobs WHERE status = 'pending' AND executor = 'go'`); n != 3 {
		t.Errorf("pending go jobs = %d, want 3 (pending, the requeued processing row, the one whose tenant came from its payload)", n)
	}
	if n := scalar[int](t, p, `SELECT count(*) FROM jobs WHERE status = 'processing' OR (status = 'pending' AND executor = 'python')`); n != 0 {
		t.Errorf("%d rows left for a worker that no longer exists", n)
	}
	if e := scalar[string](t, p, `SELECT error FROM jobs WHERE status = 'failed'`); !strings.Contains(e, "no workspace_id") {
		t.Errorf("failed job error %q", e)
	}
	if ws := scalar[string](t, p, `SELECT workspace_id FROM jobs WHERE type = 'send' AND status = 'pending'`); ws != "ws-v3" {
		t.Errorf("tenant taken from the payload = %q", ws)
	}

	// A second run is a no-op.
	if again := run(t, dsn); again.Bridged || len(again.Applied) != 0 {
		t.Errorf("second run %+v", again)
	}
}

func TestBridgeIsAtomic(t *testing.T) {
	ctx := context.Background()
	dsn := scratchDB(t)
	p := openPool(t, dsn)
	loadV3Schema(t, p)
	// A table the bridge creates already exists: the bridge must fail and leave the v3.0.0 schema and stamp alone.
	if _, err := p.Exec(ctx, `CREATE TABLE chat_messages (id int)`); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Run(ctx, openDB(t, dsn), migrate.Options{AppRole: appRole}); err == nil {
		t.Fatal("expected the bridge to fail")
	}
	if v := scalar[string](t, p, `SELECT version_num FROM alembic_version`); v != migrate.BridgeFromRevision {
		t.Errorf("stamp moved to %q after a failed bridge", v)
	}
	if scalar[bool](t, p, `SELECT to_regclass('public.workspaces') IS NOT NULL`) {
		t.Error("partial bridge DDL was committed")
	}
	if scalar[bool](t, p, `SELECT to_regclass('goose_db_version') IS NOT NULL`) {
		t.Error("goose table created after a failed bridge")
	}
	if scalar[bool](t, p, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'jobs' AND column_name = 'executor')`) {
		t.Error("jobs.executor was committed by a failed bridge")
	}
}

func TestRunStillRefusesOtherRevisionsWithBothStartsNamed(t *testing.T) {
	dsn := scratchDB(t)
	p := openPool(t, dsn)
	loadV3Schema(t, p)
	if _, err := p.Exec(context.Background(), `UPDATE alembic_version SET version_num = '2e3f40516273'`); err != nil {
		t.Fatal(err)
	}
	_, err := migrate.Run(context.Background(), openDB(t, dsn), migrate.Options{AppRole: appRole})
	if err == nil || !strings.Contains(err.Error(), "2e3f40516273") || !strings.Contains(err.Error(), migrate.BridgeFromRevision) || !strings.Contains(err.Error(), migrate.BaselineAlembicRevision) {
		t.Fatalf("err = %v", err)
	}
}
