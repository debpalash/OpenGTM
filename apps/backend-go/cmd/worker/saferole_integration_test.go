//go:build integration

package main

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// The worker refuses to start on a superuser/BYPASSRLS role like api/mcp/
// scheduler/seed: RLS would be inert for every job handler.
func TestWorkerStartupRefusesUnsafeRole(t *testing.T) {
	app := testpg.NewAppPool(t, 2)
	ctx := context.Background()
	log := telemetry.NewLogger(io.Discard)
	if err := checkDBRole(ctx, app, true, log); err != nil {
		t.Fatalf("app role refused: %v", err)
	}
	owner, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	var super, bypass bool
	if err := owner.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	if !super && !bypass {
		t.Skip("TEST_DATABASE_URL role is not superuser/BYPASSRLS")
	}
	if err := checkDBRole(ctx, owner, true, log); err == nil {
		t.Fatal("strict: superuser role accepted")
	}
	if err := checkDBRole(ctx, owner, false, log); err != nil {
		t.Fatalf("non-strict: want CRITICAL log only, got %v", err)
	}
}
