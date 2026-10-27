package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

const messageLookupFn = "public.outreach_send_by_message_id(text)"

// baselineUp is goose version 1. On an Alembic database already at the
// baseline revision it only grants the app role read access to the goose table
// (adoption); otherwise it builds the whole baseline schema, stamps
// alembic_version and applies the grants Alembic made (b3c4d5e6f7a8 runtime
// role reconciliation, a7c1e9d3b5f2/e7b2c4d6f8a0 message lookup).
func baselineUp(role string) func(context.Context, *sql.Tx) error {
	q := `"` + role + `"` // validRole guarantees a plain identifier
	return func(ctx context.Context, tx *sql.Tx) error {
		var alembic bool
		if err := tx.QueryRowContext(ctx, `SELECT to_regclass('public.alembic_version') IS NOT NULL`).Scan(&alembic); err != nil {
			return err
		}
		if alembic {
			var n int
			var rev string
			if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(min(version_num), '') FROM public.alembic_version`).Scan(&n, &rev); err != nil {
				return err
			}
			if n != 1 || rev != BaselineAlembicRevision {
				return fmt.Errorf("migrate: alembic_version is %q (%d rows), want %q", rev, n, BaselineAlembicRevision)
			}
			return run(ctx, tx,
				// Alembic's default privileges cover tables created by its own role; be explicit
				// in case the Go runner connects as a different owner.
				`DO $$ BEGIN
				   IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+role+`') THEN
				     GRANT SELECT ON public.`+VersionTable+` TO `+q+`;
				   END IF;
				 END $$`)
		}
		return run(ctx, tx,
			`SET LOCAL check_function_bodies = off`, // the dump defines SQL functions before the tables they read
			baselineSQL,
			`INSERT INTO public.alembic_version (version_num) VALUES ('`+BaselineAlembicRevision+`')`,
			// Role creation is cluster-wide; a concurrent run on another database may win the race.
			`DO $$ BEGIN
			   IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+role+`') THEN
			     CREATE ROLE `+q+` NOLOGIN NOSUPERUSER NOBYPASSRLS INHERIT;
			   END IF;
			 EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
			 END $$`,
			`GRANT USAGE ON SCHEMA public TO `+q,
			`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO `+q,
			`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO `+q,
			`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO `+q,
			`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO `+q,
			`REVOKE ALL ON FUNCTION `+messageLookupFn+` FROM PUBLIC`,
			`GRANT EXECUTE ON FUNCTION `+messageLookupFn+` TO `+q,
		)
	}
}

func run(ctx context.Context, tx *sql.Tx, stmts ...string) error {
	for _, s := range stmts {
		// No arguments: pgx uses the simple protocol, so multi-statement SQL works.
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("migrate: baseline: %w", err)
		}
	}
	return nil
}
