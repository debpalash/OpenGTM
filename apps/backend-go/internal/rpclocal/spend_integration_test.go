//go:build integration

package rpclocal

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/spend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type countRunner struct {
	n   atomic.Int32
	err error
}

func (r *countRunner) RunLocal(_ context.Context, _, _ string, _ providers.Input, _ time.Duration) (providers.Output, error) {
	r.n.Add(1)
	if r.err != nil {
		return providers.Output{}, r.err
	}
	extra := providers.NewObj()
	extra.Set("email", "a@acme.test")
	return providers.Output{Value: "a@acme.test", Confidence: 0.9, Extra: extra}, nil
}

type attemptRow struct {
	Provider, Status, Workbook string
	Reserved                   int64
	Settled                    *int64
}

func attempts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws string) []attemptRow {
	t.Helper()
	var out []attemptRow
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT provider, status, workbook_id, reserved_microusd, settled_microusd FROM workbook_spend_attempts ORDER BY created_at`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a attemptRow
			if err := rows.Scan(&a.Provider, &a.Status, &a.Workbook, &a.Reserved, &a.Settled); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func spendFixture(t *testing.T) (*Caller, *countRunner, context.Context, *pgxpool.Pool) {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	ctx := context.Background()
	clean := func() {
		_ = tenancy.InWorkspace(ctx, pool, "ws1", func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM workbook_spend_attempts`)
			return err
		})
	}
	clean()
	t.Cleanup(clean)
	r := &countRunner{}
	c := New(Deps{Local: only("hunter_io", "sec_edgar"), Runner: r, SpendPool: pool})
	return c, r, ctx, pool
}

// A paid provider call under a spend scope is an attempt of the workspace ledger: reserved at the catalog price
// before the call and settled from the answer after it.
func TestPaidScopedCallReservesAndSettles(t *testing.T) {
	c, r, ctx, pool := spendFixture(t)
	ctx = provcall.WithSpendScope(ctx, provcall.SpendScope{Run: "mcp:enrich_company:1", Row: "company:acme.test"})
	res, err := c.Call(ctx, call("hunter_io", "p1", `{"company":"Acme","domain":"acme.test"}`))
	if err != nil || res.Status != "ok" || r.n.Load() != 1 {
		t.Fatalf("res=%+v err=%v calls=%d", res, err, r.n.Load())
	}
	got := attempts(t, ctx, pool, "ws1")
	want := int64(40000) // hunter_io: $0.04
	if len(got) != 1 || got[0].Workbook != spend.WorkspaceLedger || got[0].Status != "settled" || got[0].Reserved != want ||
		got[0].Settled == nil || *got[0].Settled != want || got[0].Provider != "hunter_io" {
		t.Fatalf("attempts %+v", got)
	}
}

func TestPaidScopedCallOverTheLimitIsRefusedBeforeTheCall(t *testing.T) {
	c, r, ctx, pool := spendFixture(t)
	ctx = provcall.WithSpendScope(ctx, provcall.SpendScope{Run: "r", Row: "x", LimitUSD: 0.01})
	res, err := c.Call(ctx, call("hunter_io", "p1", `{"company":"Acme"}`))
	if err != nil || res.Status != "error" || res.Error != "spend_cell_budget" || r.n.Load() != 0 {
		t.Fatalf("res=%+v err=%v calls=%d", res, err, r.n.Load())
	}
	if got := attempts(t, ctx, pool, "ws1"); len(got) != 0 {
		t.Fatalf("a refused call left attempts %+v", got)
	}
}

func TestPaidScopedCallFailsClosedWithoutALedger(t *testing.T) {
	r := &countRunner{}
	c := New(Deps{Local: only("hunter_io"), Runner: r})
	ctx := provcall.WithSpendScope(context.Background(), provcall.SpendScope{Run: "r", Row: "x"})
	res, err := c.Call(ctx, call("hunter_io", "p1", `{"company":"Acme"}`))
	if err != nil || res.Status != "error" || res.Error != "spend_ledger_unavailable" || r.n.Load() != 0 {
		t.Fatalf("res=%+v err=%v calls=%d", res, err, r.n.Load())
	}
}

func TestFreeAndUnscopedCallsAreNotLedgered(t *testing.T) {
	c, r, ctx, pool := spendFixture(t)
	scoped := provcall.WithSpendScope(ctx, provcall.SpendScope{Run: "r", Row: "x"})
	if _, err := c.Call(scoped, call("sec_edgar", "f1", `{"company":"Acme Inc"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, call("hunter_io", "u1", `{"company":"Acme"}`)); err != nil { // workbook-style: no scope
		t.Fatal(err)
	}
	if r.n.Load() != 2 {
		t.Fatalf("runner calls %d", r.n.Load())
	}
	if got := attempts(t, ctx, pool, "ws1"); len(got) != 0 {
		t.Fatalf("attempts %+v", got)
	}
}

func TestPaidScopedCallThatFailsAfterDispatchIsUncertain(t *testing.T) {
	c, r, ctx, pool := spendFixture(t)
	r.err = errors.New("connection reset")
	ctx = provcall.WithSpendScope(ctx, provcall.SpendScope{Run: "r", Row: "x"})
	res, err := c.Call(ctx, call("hunter_io", "p1", `{"company":"Acme"}`))
	if err != nil || res.Status != "error" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if got := attempts(t, ctx, pool, "ws1"); len(got) != 1 || got[0].Status != "uncertain" {
		t.Fatalf("attempts %+v", got)
	}
}
