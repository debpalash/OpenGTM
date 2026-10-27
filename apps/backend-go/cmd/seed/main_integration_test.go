//go:build integration

package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func TestSeedExitsNotReadyBeforeSetupAndNeverCreatesAdmin(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	own, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	ctx := context.Background()
	clean := func() {
		_, _ = own.Exec(ctx, `DELETE FROM workbook_rows WHERE workbook_id = 'demo-zero-key-firstrun'`)
		_, _ = own.Exec(ctx, `DELETE FROM workbooks WHERE id = 'demo-zero-key-firstrun'`)
		_, _ = own.Exec(ctx, `DELETE FROM workspaces WHERE id = 'ws-seedcmd-test'`)
	}
	clean()
	t.Cleanup(clean)
	var users int
	_ = own.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&users)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	env := map[string]string{"SEED_WORKSPACE_SLUG": "seedcmd-test", "SEED_START_RUN": "0", "SEED_ADMIN_USERNAME": "admin", "SEED_ADMIN_PASSWORD": "admin"}
	get := func(k string) string { return env[k] }
	if code := seed(ctx, pool, config.APIConfig{SecretKey: "k"}, get, log); code != exitNotReady {
		t.Fatalf("exit = %d, want %d (no workspace before setup)", code, exitNotReady)
	}
	if !strings.Contains(logs.String(), "ignored") {
		t.Fatalf("the Python admin variables must be reported as ignored: %s", logs.String())
	}
	var n int
	_ = own.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	if n != users {
		t.Fatalf("users %d -> %d: the seed must never create an admin", users, n)
	}
	if _, err := own.Exec(ctx, `INSERT INTO workspaces (id, name, slug, description, icon, created_at, updated_at) VALUES ('ws-seedcmd-test','D','seedcmd-test','','x',0,0)`); err != nil {
		t.Fatal(err)
	}
	log = slog.New(slog.NewTextHandler(io.Discard, nil))
	if code := seed(ctx, pool, config.APIConfig{SecretKey: "k"}, get, log); code != 0 {
		t.Fatalf("exit = %d after setup", code)
	}
	if code := seed(ctx, pool, config.APIConfig{SecretKey: "k"}, get, log); code != 0 { // rerun
		t.Fatalf("rerun exit = %d", code)
	}
	_ = own.QueryRow(ctx, `SELECT count(*) FROM workbook_rows WHERE workbook_id = 'demo-zero-key-firstrun'`).Scan(&n)
	if n != 25 {
		t.Fatalf("%d rows after rerun", n)
	}
}
