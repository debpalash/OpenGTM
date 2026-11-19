package migrate

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
)

// BridgeFromRevision is the Alembic head of the v3.0.0 Python release, the only older revision the runner
// upgrades itself.
const BridgeFromRevision = "3f4051627384"

//go:embed bridge/3f4051627384.sql
var bridgeDDL string

//go:embed bridge/3f4051627384_jobs.sql
var bridgeJobs string

// applyBridge upgrades a database at Alembic 3f4051627384 (v3.0.0) to the baseline revision e7b2c4d6f8a0 in one
// transaction: the eight Python revisions between them rendered to SQL (bridge/3f4051627384.sql, pure DDL), the
// queue rows moved onto the Go worker (bridge/3f4051627384_jobs.sql), and the alembic_version stamp. Nothing is
// committed unless every statement succeeds. Afterwards the normal adoption (goose version 1) continues.
//
// Rows are never deleted or rewritten except the queue steps documented in the SQL file. The identity-plane and
// settings data that v3.0.0 kept in SQLite files is not touched here: `opengtm-migrate import-v3` copies it.
func applyBridge(ctx context.Context, db *sql.DB, role string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate: bridge: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := `"` + role + `"` // validRole guarantees a plain identifier
	ddl := strings.ReplaceAll(bridgeDDL, "{{APP_ROLE}}", q)
	if err := run(ctx, tx,
		// Role creation is cluster-wide; a concurrent run on another database may win the race.
		`DO $$ BEGIN
		   IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+role+`') THEN
		     CREATE ROLE `+q+` NOLOGIN NOSUPERUSER NOBYPASSRLS INHERIT;
		   END IF;
		 EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
		 END $$`,
		ddl,
		bridgeJobs,
	); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE public.alembic_version SET version_num = $1 WHERE version_num = $2`,
		BaselineAlembicRevision, BridgeFromRevision)
	if err != nil {
		return fmt.Errorf("migrate: bridge: stamp: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("migrate: bridge: alembic_version changed under the migration (%d rows stamped)", n)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate: bridge: commit: %w", err)
	}
	return nil
}
