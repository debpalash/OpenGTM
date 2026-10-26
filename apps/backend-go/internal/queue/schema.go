package queue

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
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

// AssertSafeRole verifies the connection role is neither superuser nor
// BYPASSRLS (either silently disables Row-Level Security). It mirrors Python's
// assert_rls_role: strict (PG_RLS_REQUIRE_SAFE_ROLE=true, the default) returns
// an error for an unsafe role; non-strict returns safe=false with no error so
// the caller can log it. Errors never include the connection string.
func AssertSafeRole(ctx context.Context, pool *pgxpool.Pool, strict bool) (bool, error) {
	var user string
	var super, bypass bool
	err := pool.QueryRow(ctx,
		`SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`,
	).Scan(&user, &super, &bypass)
	if errors.Is(err, pgx.ErrNoRows) {
		if strict {
			return false, errors.New("queue: refusing to start: could not resolve current_user in pg_roles")
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("queue: role check: %w", err)
	}
	if super || bypass {
		if strict {
			return false, fmt.Errorf("queue: refusing to start: database role %q is superuser=%t bypassrls=%t, which makes Row-Level Security inert; connect as a dedicated non-super NOBYPASSRLS role (or set PG_RLS_REQUIRE_SAFE_ROLE=false, NOT recommended)", user, super, bypass)
		}
		return false, nil
	}
	return true, nil
}
