//go:build integration

package chatmemory_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMain removes the ws-t9-* memories these tests create so they never leak into other packages.
func TestMain(m *testing.M) {
	code := m.Run()
	if url := os.Getenv("TEST_DATABASE_URL"); url != "" {
		ctx := context.Background()
		if o, err := pgxpool.New(ctx, url); err == nil {
			_, _ = o.Exec(ctx, `DELETE FROM chat_memories WHERE user_id LIKE 'ws-t9-%'`)
			o.Close()
		}
	}
	os.Exit(code)
}
