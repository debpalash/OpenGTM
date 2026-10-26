//go:build integration

package queue_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func TestAssertSafeRoleAcceptsAppRole(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	safe, err := queue.AssertSafeRole(context.Background(), pool, true)
	if err != nil || !safe {
		t.Fatalf("app role: safe=%v err=%v", safe, err)
	}
}

func TestAssertSafeRoleRefusesSuperuser(t *testing.T) {
	pool := testpg.NewAppPool(t, 2) // provisions roles/skips without a DB
	_ = pool
	ownerURL := os.Getenv("TEST_DATABASE_URL")
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, ownerURL)
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
	safe, err := queue.AssertSafeRole(ctx, owner, true)
	if err == nil || safe {
		t.Fatalf("strict: want refusal, got safe=%v err=%v", safe, err)
	}
	if strings.Contains(err.Error(), "postgres://") || strings.Contains(err.Error(), ownerURL) {
		t.Errorf("error leaks DSN: %v", err)
	}
	// Escape hatch (PG_RLS_REQUIRE_SAFE_ROLE=false): no error, but reported unsafe.
	safe, err = queue.AssertSafeRole(ctx, owner, false)
	if err != nil || safe {
		t.Fatalf("non-strict: want safe=false err=nil, got safe=%v err=%v", safe, err)
	}
}
