//go:build integration

package testpg

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Concurrent role-setup statements (as from parallel suites) must not fail with
// "tuple concurrently updated". Probabilistic without the retry, deterministic with it.
func TestExecRetryingSurvivesConcurrentAlterRole(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='`+AppLoginRole+`') THEN CREATE ROLE `+AppLoginRole+` LOGIN NOSUPERUSER NOBYPASSRLS; END IF; END $$`); err != nil {
		t.Skipf("cannot create role: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := execRetrying(ctx, pool, fmt.Sprintf(`ALTER ROLE %s PASSWORD '%s'`, AppLoginRole, appLoginPassword)); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent ALTER ROLE: %v", err)
	}
}
