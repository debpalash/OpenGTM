package queue

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// CheckSchema refuses to proceed unless alembic_version equals required.
func CheckSchema(ctx context.Context, pool *pgxpool.Pool, required string) error {
	got, err := gen.New(pool).AlembicVersion(ctx)
	if err != nil {
		return fmt.Errorf("queue: read alembic_version: %w", err)
	}
	if got != required {
		return fmt.Errorf("queue: schema revision %q does not match required %q; run migrations", got, required)
	}
	return nil
}
