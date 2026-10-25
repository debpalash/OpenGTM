// Package testpg prepares a Postgres database for integration tests: it
// creates the non-superuser, NOBYPASSRLS login role used to exercise FORCE RLS
// (mirrors legacy/python/tests/pg_rls_support.py) and returns a pool for it.
package testpg

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	AppLoginRole     = "app_rls_test"
	appLoginPassword = "rls_test_only"
)

// NewAppPool reads TEST_DATABASE_URL (owner credentials, schema already
// migrated), provisions the RLS test role, and returns a pool connected as that
// role with the given MaxConns. The test is skipped when the variable is unset.
func NewAppPool(t testing.TB, maxConns int32) *pgxpool.Pool {
	t.Helper()
	ownerURL := os.Getenv("TEST_DATABASE_URL")
	if ownerURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatalf("owner pool: %v", err)
	}
	defer owner.Close()
	stmts := []string{
		`DO $$ BEGIN
		   IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='yupcha_app') THEN CREATE ROLE yupcha_app NOLOGIN; END IF;
		   IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='` + AppLoginRole + `') THEN
		     CREATE ROLE ` + AppLoginRole + ` LOGIN NOSUPERUSER NOBYPASSRLS; END IF;
		 END $$`,
		fmt.Sprintf(`ALTER ROLE %s PASSWORD '%s'`, AppLoginRole, appLoginPassword),
		`GRANT yupcha_app TO ` + AppLoginRole,
		`GRANT USAGE ON SCHEMA public TO ` + AppLoginRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + AppLoginRole,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ` + AppLoginRole,
	}
	for _, s := range stmts {
		if _, err := owner.Exec(ctx, s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	u, err := url.Parse(ownerURL)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.User = url.UserPassword(AppLoginRole, appLoginPassword)
	cfg, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		t.Fatalf("pool config: %v", err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("app pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
