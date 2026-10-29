//go:build integration

package api

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// setupTables are the identity tables the first-admin flow reads and writes.
var setupTables = []string{"users", "workspaces", "workspace_members", "user_active_workspace", "app_settings"}

// newIsolatedIdentityPool returns an app-role pool whose search_path starts with a throwaway schema
// holding private, EMPTY copies of the identity tables (defaults, indexes and the foreign keys among
// them are cloned; sequences stay shared). Setup needs an empty users table, so those tests must never
// touch the shared public tables other suites use. The schema is dropped at the end of the test.
// (CREATE DATABASE ... TEMPLATE is not usable here: suites waiting on the cross-package advisory lock
// keep connections open to the template database, which makes Postgres refuse the copy.)
func newIsolatedIdentityPool(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	base := testpg.NewAppPool(t, 2) // skips without TEST_DATABASE_URL; holds the cross-suite lock for the test
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("owner pool: %v", err)
	}
	schema := fmt.Sprintf("setup_it_%08x", rand.Uint32())
	t.Cleanup(func() {
		_, _ = owner.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
		owner.Close()
	})
	conn, err := owner.Acquire(ctx)
	if err != nil {
		t.Fatalf("owner conn: %v", err)
	}
	defer conn.Release()
	stmts := []string{`CREATE SCHEMA ` + schema, `SET search_path TO ` + schema + `, public`}
	for _, tb := range setupTables {
		stmts = append(stmts, fmt.Sprintf(`CREATE TABLE %s.%s (LIKE public.%s INCLUDING ALL)`, schema, tb, tb))
	}
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("isolate %q: %v", s, err)
		}
	}
	// Foreign keys among the cloned tables (LIKE does not copy them); unqualified targets resolve to the clone.
	rows, err := conn.Query(ctx, `SELECT conrelid::regclass::text, conname, pg_get_constraintdef(oid)
		FROM pg_constraint WHERE contype = 'f' AND connamespace = 'public'::regnamespace
		  AND conrelid::regclass::text = ANY($1) AND confrelid::regclass::text = ANY($1)`, setupTables)
	if err != nil {
		t.Fatalf("list fks: %v", err)
	}
	type fk struct{ table, name, def string }
	var fks []fk
	for rows.Next() {
		var f fk
		if err := rows.Scan(&f.table, &f.name, &f.def); err != nil {
			t.Fatal(err)
		}
		fks = append(fks, f)
	}
	rows.Close()
	for _, f := range fks {
		if _, err := conn.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s.%s ADD CONSTRAINT %s %s`, schema, f.table, f.name, f.def)); err != nil {
			t.Fatalf("clone fk %s: %v", f.name, err)
		}
	}
	for _, s := range []string{
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + testpg.AppLoginRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ` + schema + ` TO ` + testpg.AppLoginRole,
	} {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("grant %q: %v", s, err)
		}
	}
	cfg := base.Config().Copy()
	cfg.MaxConns = maxConns
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("isolated pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
