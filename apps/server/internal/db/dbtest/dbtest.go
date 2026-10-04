// Package dbtest provides PostgreSQL fixtures for integration tests.
//
// Tests are gated on OPENGTM_TEST_DATABASE_URL, an owner (superuser) URL for a
// disposable database. The owner connection only performs setup; assertions
// run through a NOSUPERUSER NOBYPASSRLS login role so row-level security is
// really enforced, exactly as it is for the production runtime role.
package dbtest

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/config"
)

// EnvURL is the environment variable holding the owner URL.
const EnvURL = "OPENGTM_TEST_DATABASE_URL"

// AppGroupRole is the NOLOGIN runtime group the Alembic migrations grant to.
const AppGroupRole = "yupcha_app"

// setupLock serialises schema and role setup across test binaries: `go test
// ./...` runs packages in parallel and both Alembic and CREATE ROLE race.
const setupLock = 0x6f70656e67746d // "opengtm"

// OwnerURL returns the normalized owner URL, or skips the test when unset.
func OwnerURL(t testing.TB) string {
	t.Helper()
	raw := os.Getenv(EnvURL)
	if raw == "" {
		t.Skipf("%s not set; skipping PostgreSQL integration test", EnvURL)
	}
	u, err := config.NormalizeDatabaseURL(raw)
	if err != nil {
		t.Fatalf("%s: %v", EnvURL, err)
	}
	return u
}

var migrateOnce sync.Once
var migrateErr string

// Migrate brings the database to Alembic head once per test binary.
func Migrate(t testing.TB, ownerURL string) {
	t.Helper()
	migrateOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		withSetupLock(t, ownerURL, func(*pgx.Conn) {
			cmd := exec.CommandContext(ctx, "uv", "run", "--frozen", "alembic", "upgrade", "head")
			cmd.Dir = RepoRoot(t)
			cmd.Env = append(os.Environ(), "DATABASE_URL="+SQLAlchemyURL(ownerURL))
			if out, err := cmd.CombinedOutput(); err != nil {
				migrateErr = err.Error() + "\n" + string(out)
			}
		})
	})
	if migrateErr != "" {
		t.Fatalf("alembic upgrade head failed: %s", migrateErr)
	}
}

// RoleURL ensures a NOSUPERUSER NOBYPASSRLS login role exists, optionally as
// a member of groups, and returns ownerURL rewritten to log in as it.
func RoleURL(t testing.TB, ownerURL, role, password string, groups ...string) string {
	t.Helper()
	withSetupLock(t, ownerURL, func(c *pgx.Conn) {
		ctx := context.Background()
		ident := pgx.Identifier{role}.Sanitize()
		var exists bool
		if err := c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname=$1)", role).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		stmts := []string{}
		if !exists {
			stmts = append(stmts, "CREATE ROLE "+ident+" LOGIN NOSUPERUSER NOBYPASSRLS")
		}
		stmts = append(stmts,
			"ALTER ROLE "+ident+" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '"+password+"'",
			"GRANT USAGE ON SCHEMA public TO "+ident,
		)
		for _, g := range groups {
			stmts = append(stmts, "GRANT "+pgx.Identifier{g}.Sanitize()+" TO "+ident)
		}
		for _, s := range stmts {
			if _, err := c.Exec(ctx, s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	})
	u, err := url.Parse(ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	return u.String()
}

// AppURL returns a URL for a login role that holds the production runtime
// group's privileges. The schema must already be migrated.
func AppURL(t testing.TB, ownerURL string) string {
	t.Helper()
	return RoleURL(t, ownerURL, "opengtm_core_app_test", "core_app_test_only", AppGroupRole)
}

// Pool opens a pool closed at test cleanup.
func Pool(t testing.TB, dsn string, maxConns int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// SQLAlchemyURL converts a postgresql:// URL to the psycopg form Alembic uses.
func SQLAlchemyURL(u string) string {
	if rest, ok := strings.CutPrefix(u, "postgresql://"); ok {
		return "postgresql+psycopg://" + rest
	}
	return u
}

// RepoRoot locates the repository root (the directory with alembic.ini).
func RepoRoot(t testing.TB) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "alembic.ini")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("dbtest: alembic.ini not found above " + file)
		}
		dir = parent
	}
}

func withSetupLock(t testing.TB, ownerURL string, fn func(*pgx.Conn)) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		t.Fatalf("dbtest: connect owner: %v", err)
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(setupLock)); err != nil {
		t.Fatal(err)
	}
	defer c.Exec(ctx, "SELECT pg_advisory_unlock($1)", int64(setupLock))
	fn(c)
}
