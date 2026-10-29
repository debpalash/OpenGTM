//go:build integration

package tenancy_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func countWorkbooks(ctx context.Context, tx pgx.Tx, id string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM workbooks WHERE id = $1`, id).Scan(&n)
	return n, err
}

func TestInWorkspaceIsolatesTenants(t *testing.T) {
	ctx := context.Background()
	pool := testpg.NewAppPool(t, 1) // single conn: GUC must not leak between txs
	suffix := t.Name()
	wsA, wsB := "ws-a-"+suffix, "ws-b-"+suffix
	wbID := "wb-" + suffix
	t.Cleanup(func() {
		_ = tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM workbooks WHERE id = $1`, wbID)
			return err
		})
	})

	err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workbooks (id, name, workspace_id) VALUES ($1, 'wb', $2)`, wbID, wsA)
		return err
	})
	if err != nil {
		t.Fatalf("insert as A: %v", err)
	}
	for i := 0; i < 50; i++ {
		ws, want := wsB, 0
		if i%2 == 0 {
			ws, want = wsA, 1
		}
		var got int
		if err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) (e error) {
			got, e = countWorkbooks(ctx, tx, wbID)
			return
		}); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if got != want {
			t.Fatalf("iter %d ws=%s: got %d rows, want %d", i, ws, got, want)
		}
	}
}

func TestUnscopedQueryFailsClosed(t *testing.T) {
	ctx := context.Background()
	pool := testpg.NewAppPool(t, 1)
	wsA := "ws-a-" + t.Name()
	wbID := "wb-" + t.Name()
	if err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workbooks (id, name, workspace_id) VALUES ($1, 'wb', $2)`, wbID, wsA)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM workbooks WHERE id = $1`, wbID)
			return err
		})
	})
	rows, err := pool.Query(ctx, `SELECT id FROM workbooks`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	if n != 0 {
		t.Fatalf("unscoped query returned %d rows, want 0", n)
	}
}

func TestEmptyWorkspaceRejected(t *testing.T) {
	pool := testpg.NewAppPool(t, 1)
	called := false
	err := tenancy.InWorkspace(context.Background(), pool, "", func(pgx.Tx) error {
		called = true
		return nil
	})
	if !errors.Is(err, tenancy.ErrNoWorkspace) {
		t.Fatalf("err = %v, want ErrNoWorkspace", err)
	}
	if called {
		t.Fatal("fn must not run")
	}
}

func TestRollbackOnError(t *testing.T) {
	ctx := context.Background()
	pool := testpg.NewAppPool(t, 1)
	ws := "ws-" + t.Name()
	wbID := "wb-" + t.Name()
	boom := fmt.Errorf("boom")
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO workbooks (id, name, workspace_id) VALUES ($1, 'wb', $2)`, wbID, ws); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	var got int
	if err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) (e error) {
		got, e = countWorkbooks(ctx, tx, wbID)
		return
	}); err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("row survived rollback: %d", got)
	}
}
