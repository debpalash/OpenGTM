//go:build integration

package billing

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func ledgerPool(t *testing.T, ws ...string) *pgxpool.Pool {
	t.Helper()
	pool := testpg.NewAppPool(t, 24)
	clean := func() {
		for _, w := range ws {
			_ = tenancy.InWorkspace(context.Background(), pool, w, func(tx pgx.Tx) error {
				_, _ = tx.Exec(context.Background(), `DELETE FROM credit_ledger_entries WHERE workspace_id=$1`, w)
				_, _ = tx.Exec(context.Background(), `DELETE FROM workspace_credits WHERE workspace_id=$1`, w)
				return nil
			})
		}
	}
	clean()
	t.Cleanup(clean)
	return pool
}

func credit(t *testing.T, pool *pgxpool.Pool, ws string, amt float64, reason, key string) DebitResult {
	t.Helper()
	var res DebitResult
	err := tenancy.InWorkspace(context.Background(), pool, ws, func(tx pgx.Tx) (e error) {
		res, e = Credit(context.Background(), tx, ws, amt, reason, key)
		return e
	})
	if err != nil {
		t.Fatalf("credit: %v", err)
	}
	return res
}

func TestCreditCreatesBalanceAndEntry(t *testing.T) {
	pool := ledgerPool(t, "t9-led-a")
	r := credit(t, pool, "t9-led-a", 25, "stripe_topup", "stripe:cs_1")
	if r.ChargedUSD != 25 || r.BalanceUSD != 25 || r.IdempotentReplay {
		t.Fatalf("result %+v", r)
	}
	r = credit(t, pool, "t9-led-a", 0.1, "manual_grant", "")
	if r.ChargedUSD != 0.1 || r.BalanceUSD != 25.1 || r.IdempotentReplay {
		t.Fatalf("result %+v", r)
	}
	// Keyless credits are never replays of each other.
	r = credit(t, pool, "t9-led-a", 0.1, "manual_grant", "")
	if r.IdempotentReplay || r.BalanceUSD != 25.2 {
		t.Fatalf("keyless second credit: %+v", r)
	}
}

func TestCreditRejectsNonPositive(t *testing.T) {
	pool := ledgerPool(t, "t9-led-b")
	for _, amt := range []float64{0, -1} {
		err := tenancy.InWorkspace(context.Background(), pool, "t9-led-b", func(tx pgx.Tx) error {
			_, e := Credit(context.Background(), tx, "t9-led-b", amt, "x", "k")
			return e
		})
		if !errors.Is(err, ErrNonPositiveCredit) {
			t.Errorf("amount %v: %v", amt, err)
		}
	}
}

func TestCreditIdempotentReplay(t *testing.T) {
	pool := ledgerPool(t, "t9-led-c")
	first := credit(t, pool, "t9-led-c", 10, "stripe_topup", "stripe:cs_replay")
	second := credit(t, pool, "t9-led-c", 10, "stripe_topup", "stripe:cs_replay")
	if first.IdempotentReplay || !second.IdempotentReplay {
		t.Fatalf("replay flags: %+v %+v", first, second)
	}
	if second.ChargedUSD != 0 || second.BalanceUSD != 10 {
		t.Fatalf("replay result %+v", second)
	}
	var n int
	_ = tenancy.InWorkspace(context.Background(), pool, "t9-led-c", func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM credit_ledger_entries WHERE idempotency_key='stripe:cs_replay'`).Scan(&n)
	})
	if n != 1 {
		t.Fatalf("%d ledger rows", n)
	}
}

func TestCreditConcurrentSameKeyOnlyOnce(t *testing.T) {
	pool := ledgerPool(t, "t9-led-d")
	const n = 16
	var wg sync.WaitGroup
	results := make([]DebitResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = tenancy.InWorkspace(context.Background(), pool, "t9-led-d", func(tx pgx.Tx) (e error) {
				results[i], e = Credit(context.Background(), tx, "t9-led-d", 7.5, "stripe_topup", "stripe:cs_race")
				return e
			})
		}()
	}
	close(start)
	wg.Wait()
	applied := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if !results[i].IdempotentReplay {
			applied++
		}
		if results[i].BalanceUSD != 7.5 {
			t.Errorf("goroutine %d balance %v", i, results[i].BalanceUSD)
		}
	}
	if applied != 1 {
		t.Fatalf("%d goroutines applied the credit, want 1", applied)
	}
	var rows int
	var bal float64
	_ = tenancy.InWorkspace(context.Background(), pool, "t9-led-d", func(tx pgx.Tx) error {
		if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM credit_ledger_entries WHERE idempotency_key='stripe:cs_race'`).Scan(&rows); err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), `SELECT balance_usd FROM workspace_credits WHERE workspace_id='t9-led-d'`).Scan(&bal)
	})
	if rows != 1 || bal != 7.5 {
		t.Fatalf("rows=%d balance=%v", rows, bal)
	}
}

func TestCreditSameKeyOtherTenantIsReplayNotLeak(t *testing.T) {
	pool := ledgerPool(t, "t9-led-e1", "t9-led-e2")
	credit(t, pool, "t9-led-e1", 5, "stripe_topup", "stripe:cs_shared")
	r := credit(t, pool, "t9-led-e2", 5, "stripe_topup", "stripe:cs_shared")
	if !r.IdempotentReplay || r.ChargedUSD != 0 || r.BalanceUSD != 0 {
		t.Fatalf("other tenant: %+v", r)
	}
}

func TestBalanceRoundingAndEntries(t *testing.T) {
	pool := ledgerPool(t, "t9-led-f")
	credit(t, pool, "t9-led-f", 0.00001234, "manual_grant", "")
	credit(t, pool, "t9-led-f", 24.87654, "manual_grant", "")
	err := tenancy.InWorkspace(context.Background(), pool, "t9-led-f", func(tx pgx.Tx) error {
		bal, err := Balance(context.Background(), tx, "t9-led-f")
		if err != nil {
			return err
		}
		if bal != 24.8766 {
			t.Errorf("balance %v", bal)
		}
		es, err := RecentEntries(context.Background(), tx, "t9-led-f", 50)
		if err != nil {
			return err
		}
		if len(es) != 2 || es[0].ID <= es[1].ID || es[0].AmountUSD != 24.8765 {
			t.Errorf("entries %+v", es)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBalanceMissingRowIsZero(t *testing.T) {
	pool := ledgerPool(t, "t9-led-g")
	_ = tenancy.InWorkspace(context.Background(), pool, "t9-led-g", func(tx pgx.Tx) error {
		bal, err := Balance(context.Background(), tx, "t9-led-g")
		if err != nil || bal != 0 {
			t.Errorf("balance %v err %v", bal, err)
		}
		return nil
	})
}
