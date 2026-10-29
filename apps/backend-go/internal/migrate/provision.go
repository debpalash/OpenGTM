package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// provisionRuntimeLogin mirrors legacy/python provision_runtime_role: create or
// update the login (LOGIN INHERIT NOSUPERUSER NOBYPASSRLS, password), make it a
// member of the app group, grant CONNECT on the current database and USAGE on
// public. Identifiers and the password are quoted server-side with format().
func provisionRuntimeLogin(ctx context.Context, db *sql.DB, group string, rl RuntimeLogin) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate: provision: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var ok bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, group).Scan(&ok); err != nil {
		return fmt.Errorf("migrate: provision: %w", err)
	}
	if !ok {
		return fmt.Errorf("migrate: provision: application group role %q is missing; the baseline creates it", group)
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, rl.User).Scan(&ok); err != nil {
		return fmt.Errorf("migrate: provision: %w", err)
	}
	verb := "CREATE"
	if ok {
		verb = "ALTER"
	}
	steps := []string{
		verb + ` ROLE %1$I LOGIN INHERIT NOSUPERUSER NOBYPASSRLS PASSWORD %2$L`,
		`GRANT %3$I TO %1$I`,
		`GRANT CONNECT ON DATABASE %4$I TO %1$I`,
		`GRANT USAGE ON SCHEMA public TO %1$I`,
	}
	for _, format := range steps {
		var stmt string
		if err := tx.QueryRowContext(ctx, `SELECT format($1, $2::text, $3::text, $4::text, current_database())`,
			format, rl.User, rl.Password, group).Scan(&stmt); err != nil {
			return fmt.Errorf("migrate: provision: %w", err)
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			// Never wrap the statement: it carries the password.
			return fmt.Errorf("migrate: provision runtime login %q: %w", rl.User, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate: provision: %w", err)
	}
	return nil
}
