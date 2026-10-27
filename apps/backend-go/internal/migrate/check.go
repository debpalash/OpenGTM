package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CurrentVersion returns the highest applied goose version.
func CurrentVersion(ctx context.Context, q Querier) (int64, error) {
	var v int64
	err := q.QueryRow(ctx, `SELECT coalesce(max(version_id), 0) FROM public.`+VersionTable).Scan(&v)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return 0, fmt.Errorf("migrate: %s is missing: this database has not been migrated by the Go runner; "+
			"run opengtm-migrate (owner credentials) first", VersionTable)
	}
	if err != nil {
		return 0, fmt.Errorf("migrate: read %s: %w", VersionTable, err)
	}
	return v, nil
}

// CheckSchema is the api/worker/mcp startup gate: the database must have every
// migration up to required applied. A newer database is accepted (migrations
// stay additive, so a rolling deploy can run the previous binary on it).
func CheckSchema(ctx context.Context, q Querier, required int64) error {
	v, err := CurrentVersion(ctx, q)
	if err != nil {
		return err
	}
	if v < required {
		return fmt.Errorf("migrate: schema version %d is below required %d; run opengtm-migrate", v, required)
	}
	return nil
}
