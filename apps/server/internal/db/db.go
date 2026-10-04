// Package db owns PostgreSQL connectivity and the tenant transaction boundary.
//
// Tenant isolation is enforced by PostgreSQL row-level security keyed on the
// transaction-local GUC app.workspace_id (see the RLS Alembic migrations).
// Every tenant read or write must therefore run inside WithTenant; there is
// deliberately no helper that sets the GUC for a whole session, because a
// pooled connection would then carry one tenant's identity into the next
// caller's transaction.
package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoWorkspace is returned when a tenant transaction is requested without a
// workspace. Failing here is louder than letting RLS return zero rows.
var ErrNoWorkspace = errors.New("db: tenant transaction requires a workspace id")

// Beginner is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx (savepoint).
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Options tune the pool for a role.
type Options struct {
	// AppName is reported as application_name so operators can attribute
	// connections in pg_stat_activity.
	AppName  string
	MaxConns int32
	// Lazy skips the startup ping. The HTTP server uses it so it can start,
	// answer /healthz and report the outage on /readyz while PostgreSQL is
	// still coming up; workers keep the ping and fail fast instead.
	Lazy bool
}

// Open creates a pool and verifies connectivity.
func Open(ctx context.Context, url string, opts Options) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("db: parse DATABASE_URL: %w", err)
	}
	if opts.AppName != "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = opts.AppName
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: connect: %w", err)
	}
	if opts.Lazy {
		return pool, nil
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// WithTenant runs fn in a transaction bound to workspaceID. The binding uses
// set_config(..., is_local => true), so it ends with the transaction on both
// commit and rollback and can never leak to the next user of the connection.
func WithTenant(ctx context.Context, b Beginner, workspaceID string, fn func(pgx.Tx) error) error {
	if strings.TrimSpace(workspaceID) == "" {
		return ErrNoWorkspace
	}
	return pgx.BeginFunc(ctx, b, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", workspaceID); err != nil {
			return fmt.Errorf("db: bind workspace: %w", err)
		}
		return fn(tx)
	})
}

// WithoutTenant runs fn in a transaction with no workspace bound. Use it only
// for global control tables (jobs, job_executor_routes); tenant tables read
// from here fail closed and return no rows.
func WithoutTenant(ctx context.Context, b Beginner, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, b, fn)
}
