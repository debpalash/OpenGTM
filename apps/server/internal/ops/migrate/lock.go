package migrate

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// lockKey is the PostgreSQL advisory lock that makes the Go migration owner
// single. It is distinct from the internal/db/dbtest setup lock. Alembic
// itself does not take it, so it serializes opengtm runs (several replicas, an
// operator and an automated upgrade), not arbitrary external Alembic use.
const lockKey int64 = 0x6f70656e6d6967 // "openmig"

// Lock takes the cluster-wide migration lock on the database at url and
// returns its release function. It waits up to wait for another owner.
func Lock(ctx context.Context, url string, wait time.Duration) (release func(), err error) {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("migrate: connect for advisory lock: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		var got bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockKey).Scan(&got); err != nil {
			conn.Close(context.Background())
			return nil, fmt.Errorf("migrate: advisory lock: %w", err)
		}
		if got {
			break
		}
		if time.Now().After(deadline) {
			conn.Close(context.Background())
			return nil, fmt.Errorf("migrate: another migration is running against this database (waited %s)", wait)
		}
		select {
		case <-ctx.Done():
			conn.Close(context.Background())
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return func() {
		// Closing the session releases the lock even if the unlock fails.
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lockKey)
		conn.Close(context.Background())
	}, nil
}
