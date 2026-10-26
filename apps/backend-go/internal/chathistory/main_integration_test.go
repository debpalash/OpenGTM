//go:build integration

package chathistory_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMain removes the ws-t8-* rows these tests create so they never leak into other packages'
// assertions on the shared database (package order must not matter).
func TestMain(m *testing.M) {
	code := m.Run()
	if url := os.Getenv("TEST_DATABASE_URL"); url != "" {
		ctx := context.Background()
		if o, err := pgxpool.New(ctx, url); err == nil {
			for _, q := range []string{
				`DELETE FROM chat_messages WHERE workspace_id LIKE 'ws-t8-%'`,
				`DELETE FROM chat_tool_approvals WHERE workspace_id LIKE 'ws-t8-%'`,
				`DELETE FROM chat_conversations WHERE workspace_id LIKE 'ws-t8-%'`,
			} {
				_, _ = o.Exec(ctx, q)
			}
			o.Close()
		}
	}
	os.Exit(code)
}
