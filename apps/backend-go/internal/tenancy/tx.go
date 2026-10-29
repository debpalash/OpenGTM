// Package tenancy runs database work inside a transaction scoped to one
// workspace so Postgres FORCE RLS policies see the right tenant.
package tenancy

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoWorkspace is returned when InWorkspace is called with an empty ID.
var ErrNoWorkspace = errors.New("tenancy: workspace id is required")

// InWorkspace begins a transaction, sets app.workspace_id transaction-locally,
// runs fn, and commits. It rolls back if fn (or the commit) fails. Because the
// GUC is set with is_local=true it never leaks to the next user of the
// pooled connection.
func InWorkspace(ctx context.Context, pool *pgxpool.Pool, workspaceID string, fn func(pgx.Tx) error) error {
	if workspaceID == "" {
		return ErrNoWorkspace
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("tenancy: begin: %w", err)
	}
	// Rollback after a successful commit is a no-op; use a fresh context so a
	// cancelled ctx still releases the connection.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, `SELECT set_config('app.workspace_id', $1, true)`, workspaceID); err != nil {
		return fmt.Errorf("tenancy: set workspace: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tenancy: commit: %w", err)
	}
	return nil
}
