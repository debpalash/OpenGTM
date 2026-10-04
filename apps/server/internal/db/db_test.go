package db_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/db"
	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
)

func TestWithTenantRejectsEmptyWorkspace(t *testing.T) {
	for _, ws := range []string{"", "  "} {
		err := db.WithTenant(context.Background(), nil, ws, func(pgx.Tx) error {
			t.Fatal("fn must not run without a workspace")
			return nil
		})
		if !errors.Is(err, db.ErrNoWorkspace) {
			t.Fatalf("WithTenant(%q) err = %v, want ErrNoWorkspace", ws, err)
		}
	}
}

// rlsPool creates a FORCE RLS table using the production policy shape and
// returns a single-connection pool for a NOSUPERUSER NOBYPASSRLS role, so
// every call reuses the same physical connection.
func rlsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	owner := dbtest.OwnerURL(t)
	ctx := context.Background()
	appURL := dbtest.RoleURL(t, owner, "opengtm_core_rls_probe", "rls_probe_only")
	oc := dbtest.Pool(t, owner, 1)
	for _, s := range []string{
		`DROP TABLE IF EXISTS opengtm_core_rls_probe`,
		`CREATE TABLE opengtm_core_rls_probe (id serial PRIMARY KEY, workspace_id text NOT NULL, v text)`,
		`ALTER TABLE opengtm_core_rls_probe ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE opengtm_core_rls_probe FORCE ROW LEVEL SECURITY`,
		`CREATE POLICY workspace_isolation ON opengtm_core_rls_probe
		   USING (workspace_id = current_setting('app.workspace_id', true))
		   WITH CHECK (workspace_id = current_setting('app.workspace_id', true))`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON opengtm_core_rls_probe TO opengtm_core_rls_probe`,
		`GRANT USAGE, SELECT ON SEQUENCE opengtm_core_rls_probe_id_seq TO opengtm_core_rls_probe`,
	} {
		if _, err := oc.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() { oc.Exec(context.Background(), `DROP TABLE IF EXISTS opengtm_core_rls_probe`) })

	pool := dbtest.Pool(t, appURL, 1)
	var super, bypass bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatalf("probe role must be NOSUPERUSER NOBYPASSRLS (super=%v bypass=%v)", super, bypass)
	}
	for _, ws := range []string{"ws-a", "ws-b"} {
		for i := range 3 {
			err := db.WithTenant(ctx, pool, ws, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO opengtm_core_rls_probe (workspace_id, v) VALUES ($1, $2)`, ws, fmt.Sprint(i))
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	return pool
}

func visible(t *testing.T, pool *pgxpool.Pool, ws string) map[string]int {
	t.Helper()
	ctx := context.Background()
	seen := map[string]int{}
	collect := func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT workspace_id FROM opengtm_core_rls_probe`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var w string
			if err := rows.Scan(&w); err != nil {
				return err
			}
			seen[w]++
		}
		return rows.Err()
	}
	var err error
	if ws == "" {
		err = db.WithoutTenant(ctx, pool, collect)
	} else {
		err = db.WithTenant(ctx, pool, ws, collect)
	}
	if err != nil {
		t.Fatal(err)
	}
	return seen
}

func TestTenantIsolationUnderForcedRLS(t *testing.T) {
	pool := rlsPool(t)

	t.Run("bound tenant sees only its rows", func(t *testing.T) {
		for _, ws := range []string{"ws-a", "ws-b"} {
			got := visible(t, pool, ws)
			if len(got) != 1 || got[ws] != 3 {
				t.Fatalf("tenant %s saw %v", ws, got)
			}
		}
	})

	t.Run("unset context fails closed", func(t *testing.T) {
		if got := visible(t, pool, ""); len(got) != 0 {
			t.Fatalf("no workspace bound but saw %v", got)
		}
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM opengtm_core_rls_probe`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("bare pool query saw %d rows (err %v)", n, err)
		}
	})

	t.Run("pooled connection alternating tenants never leaks", func(t *testing.T) {
		for i := range 200 {
			ws := []string{"ws-a", "ws-b", ""}[i%3]
			got := visible(t, pool, ws)
			if ws == "" {
				if len(got) != 0 {
					t.Fatalf("iteration %d: unbound saw %v", i, got)
				}
				continue
			}
			if len(got) != 1 || got[ws] != 3 {
				t.Fatalf("iteration %d: tenant %s saw %v", i, ws, got)
			}
		}
	})

	t.Run("cross-tenant write rejected by WITH CHECK", func(t *testing.T) {
		ctx := context.Background()
		err := db.WithTenant(ctx, pool, "ws-a", func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO opengtm_core_rls_probe (workspace_id, v) VALUES ('ws-b', 'x')`)
			return err
		})
		if err == nil {
			t.Fatal("insert into another tenant succeeded")
		}
	})

	t.Run("errors roll back and the setting does not persist", func(t *testing.T) {
		ctx := context.Background()
		boom := errors.New("boom")
		err := db.WithTenant(ctx, pool, "ws-a", func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO opengtm_core_rls_probe (workspace_id, v) VALUES ('ws-a', 'rolled-back')`); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
		if got := visible(t, pool, "ws-a"); got["ws-a"] != 3 {
			t.Fatalf("rollback did not discard insert: %v", got)
		}
		var setting string
		if err := pool.QueryRow(ctx, `SELECT coalesce(current_setting('app.workspace_id', true), '')`).Scan(&setting); err != nil {
			t.Fatal(err)
		}
		if setting != "" {
			t.Fatalf("app.workspace_id leaked onto the connection: %q", setting)
		}
		// A panic inside fn must also roll back and release the connection.
		func() {
			defer func() { _ = recover() }()
			_ = db.WithTenant(ctx, pool, "ws-b", func(tx pgx.Tx) error { panic("handler bug") })
		}()
		if got := visible(t, pool, ""); len(got) != 0 {
			t.Fatalf("after panic, unbound saw %v", got)
		}
	})
}
