//go:build integration

package spend_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/spend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const ws = "ws-spend"

func setup(t *testing.T, budgetMax, budgetSpent *float64) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testpg.NewAppPool(t, 16)
	ctx := context.Background()
	clean := func() {
		for _, w := range []string{ws, "ws-other"} {
			_ = tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
				_, _ = tx.Exec(ctx, `DELETE FROM workbook_spend_attempts`)
				_, err := tx.Exec(ctx, `DELETE FROM workbooks`)
				return err
			})
		}
		_, _ = pool.Exec(ctx, `DELETE FROM jobs`)
	}
	clean()
	t.Cleanup(clean)
	for w, wb := range map[string]string{ws: "wb1", "ws-other": "wbo"} {
		if err := tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO workbooks (id, name, workspace_id, budget_max_usd, budget_spent_usd)
				VALUES ($4, 'wb', $1, $2, $3)`, w, budgetMax, budgetSpent, wb)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return pool, ctx
}

func f(v float64) *float64 { return &v }

func res(key string, exposure, cellLimit int64) spend.Reservation {
	return spend.Reservation{WorkspaceID: ws, WorkbookID: "wb1", RunID: "job:1", RowIdentity: "r1", ColumnID: "c1",
		Provider: "apollo", AttemptKey: key, ExposureMicroUSD: exposure, CellLimitMicroUSD: cellLimit,
		CostBasis: map[string]any{"unit_usd": 0.05}, Operation: map[string]any{"op": "enrich"}}
}

func reserve(t *testing.T, pool *pgxpool.Pool, r spend.Reservation) spend.Admission {
	t.Helper()
	var a spend.Admission
	if err := tenancy.InWorkspace(context.Background(), pool, r.WorkspaceID, func(tx pgx.Tx) error {
		var err error
		a, err = spend.Reserve(context.Background(), tx, r)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return a
}

func ident(a spend.Admission) spend.Identity {
	return spend.Identity{WorkspaceID: ws, WorkbookID: "wb1", AttemptID: a.ID, ContractHash: a.ContractHash}
}

func transition(t *testing.T, pool *pgxpool.Pool, id spend.Identity, action string) bool {
	t.Helper()
	var ok bool
	if err := tenancy.InWorkspace(context.Background(), pool, id.WorkspaceID, func(tx pgx.Tx) error {
		var err error
		ok, err = spend.Transition(context.Background(), tx, id, action)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return ok
}

func settle(t *testing.T, pool *pgxpool.Pool, id spend.Identity, amt int64, basis string, result map[string]any) spend.SettleResult {
	t.Helper()
	var s spend.SettleResult
	if err := tenancy.InWorkspace(context.Background(), pool, id.WorkspaceID, func(tx pgx.Tx) error {
		var err error
		s, err = spend.Settle(context.Background(), tx, id, amt, basis, result)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func spent(t *testing.T, pool *pgxpool.Pool) float64 {
	t.Helper()
	var v float64
	_ = tenancy.InWorkspace(context.Background(), pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT COALESCE(budget_spent_usd,0) FROM workbooks WHERE id='wb1'`).Scan(&v)
	})
	return v
}

func TestReserveIdempotentSameContract(t *testing.T) {
	pool, _ := setup(t, nil, nil)
	a := reserve(t, pool, res("k1", 100, 1000))
	if !a.OK || a.Reused || a.Status != "reserved" || a.ID == "" || len(a.ContractHash) != 64 {
		t.Fatalf("%+v", a)
	}
	b := reserve(t, pool, res("k1", 100, 1000))
	if !b.OK || !b.Reused || b.ID != a.ID || b.ContractHash != a.ContractHash {
		t.Fatalf("%+v", b)
	}
	if !transition(t, pool, ident(a), "dispatch") {
		t.Fatal("dispatch")
	}
	if c := reserve(t, pool, res("k1", 100, 1000)); !c.Reused || c.Status != "dispatched" {
		t.Fatalf("%+v", c)
	}
}

func TestReserveConflictOnChangedInputs(t *testing.T) {
	pool, _ := setup(t, nil, nil)
	reserve(t, pool, res("k1", 100, 1000))
	for name, mut := range map[string]func(*spend.Reservation){
		"exposure": func(r *spend.Reservation) { r.ExposureMicroUSD = 101 },
		"cell":     func(r *spend.Reservation) { r.CellLimitMicroUSD = 999 },
		"basis":    func(r *spend.Reservation) { r.CostBasis = map[string]any{"unit_usd": 0.06} },
		"op":       func(r *spend.Reservation) { r.Operation = map[string]any{"op": "other"} },
		"row":      func(r *spend.Reservation) { r.RowIdentity = "r2" },
	} {
		r := res("k1", 100, 1000)
		mut(&r)
		if a := reserve(t, pool, r); a.OK || a.Reason != "contract_conflict" {
			t.Errorf("%s: %+v", name, a)
		}
	}
}

func TestReserveWorkbookNotFoundAndWrongTenant(t *testing.T) {
	pool, _ := setup(t, nil, nil)
	r := res("k1", 1, 10)
	r.WorkbookID = "missing"
	if a := reserve(t, pool, r); a.OK || a.Reason != "workbook_not_found" {
		t.Fatalf("%+v", a)
	}
	// A workspace-A reservation executed in workspace B's transaction is invisible under RLS.
	r = res("k2", 1, 10)
	r.WorkspaceID, r.WorkbookID = "ws-other", "wbo"
	a := reserve(t, pool, r)
	if !a.OK {
		t.Fatalf("%+v", a)
	}
	if transition(t, pool, ident(a), "dispatch") {
		t.Fatal("cross-tenant transition must not apply")
	}
}

func TestReserveInvalidInput(t *testing.T) {
	pool, ctx := setup(t, nil, nil)
	bad := []spend.Reservation{res("", 1, 1), res("k", -1, 1), res("k", 1, -1)}
	r := res("k", 1, 1)
	r.CostBasis = nil
	bad = append(bad, r)
	r = res("k", 1, 1)
	r.Operation = map[string]any{}
	bad = append(bad, r)
	r = res("k", 1, 1)
	r.Provider = string(make([]rune, 256))
	bad = append(bad, r)
	for i, b := range bad {
		err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error { _, err := spend.Reserve(ctx, tx, b); return err })
		if err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestCellBudgetEnforced(t *testing.T) {
	pool, _ := setup(t, nil, nil)
	a := reserve(t, pool, res("k1", 60, 100))
	if !a.OK {
		t.Fatal(a)
	}
	if b := reserve(t, pool, res("k2", 60, 100)); b.OK || b.Reason != "cell_budget" {
		t.Fatalf("%+v", b)
	}
	// Settled amount (not the reservation) counts toward the cell once settled.
	transition(t, pool, ident(a), "dispatch")
	if s := settle(t, pool, ident(a), 10, "catalog_estimate", map[string]any{"ok": true}); !s.OK {
		t.Fatal(s)
	}
	if b := reserve(t, pool, res("k2", 60, 100)); !b.OK {
		t.Fatalf("%+v", b)
	}
	// Released attempts do not count; a different cell is independent.
	c := reserve(t, pool, res("k3", 31, 100))
	if c.OK {
		t.Fatalf("10+60+31 > 100 expected reject: %+v", c)
	}
	other := res("k4", 100, 100)
	other.RowIdentity = "r2"
	if d := reserve(t, pool, other); !d.OK {
		t.Fatalf("%+v", d)
	}
}

func TestWorkbookBudgetEnforced(t *testing.T) {
	pool, _ := setup(t, f(0.0001), f(0.00005)) // cap 100µ, spent 50µ
	if a := reserve(t, pool, res("k1", 51, 1000)); a.OK || a.Reason != "workbook_budget" {
		t.Fatalf("%+v", a)
	}
	a := reserve(t, pool, res("k2", 50, 1000))
	if !a.OK {
		t.Fatalf("%+v", a)
	}
	if b := reserve(t, pool, res("k3", 1, 1000)); b.OK || b.Reason != "workbook_budget" {
		t.Fatalf("outstanding reservation must count: %+v", b)
	}
	transition(t, pool, ident(a), "release_before_dispatch")
	if b := reserve(t, pool, res("k3", 1, 1000)); !b.OK {
		t.Fatalf("released exposure must free headroom: %+v", b)
	}
}

func TestWorkbookBudgetRoundingAndUnlimited(t *testing.T) {
	// Each subtest owns its pool: testpg holds a cross-test advisory lock until cleanup.
	t.Run("ceil spent floor cap", func(t *testing.T) {
		pool, _ := setup(t, f(0.0000019), f(0.0000001)) // cap floor(1.9)=1, spent ceil(0.1)=1
		if a := reserve(t, pool, res("k1", 1, 10)); a.OK || a.Reason != "workbook_budget" {
			t.Fatalf("%+v", a)
		}
		if a := reserve(t, pool, res("k2", 0, 10)); !a.OK {
			t.Fatalf("%+v", a)
		}
	})
	t.Run("cap zero is unlimited", func(t *testing.T) {
		pool, _ := setup(t, f(0), f(5))
		if a := reserve(t, pool, res("k1", 1<<40, 1<<41)); !a.OK {
			t.Fatalf("%+v", a)
		}
	})
	t.Run("negative cap invalid", func(t *testing.T) {
		pool, _ := setup(t, f(-1), nil)
		if a := reserve(t, pool, res("k1", 1, 10)); a.OK || a.Reason != "invalid_workbook_budget" {
			t.Fatalf("%+v", a)
		}
	})
}

func TestConcurrentAdmissionsSerialize(t *testing.T) {
	pool, _ := setup(t, f(0.0001), nil) // cap 100µ
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := res(fmt.Sprintf("k%d", i), 10, 1000)
			r.RowIdentity = fmt.Sprintf("row%d", i)
			if a := reserve(t, pool, r); a.OK {
				admitted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if admitted.Load() != 10 {
		t.Fatalf("admitted %d, want exactly 10", admitted.Load())
	}
}

func claimRunWorkbook(t *testing.T, pool *pgxpool.Pool, workbook string) (*queue.Claim, int64) {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO jobs (type, payload, status, priority, created_at, retry_count, max_retries, workspace_id, executor)
		VALUES ('run_workbook', json_build_object('workbook_id', $1::text), 'pending', 0, now() AT TIME ZONE 'UTC', 0, 3, $2, 'go') RETURNING id`,
		workbook, ws).Scan(&id); err != nil {
		t.Fatal(err)
	}
	c, err := queue.ClaimNext(ctx, pool, "worker-1", 0)
	if err != nil || c == nil || c.ID != id {
		t.Fatalf("claim %v %v", c, err)
	}
	return c, id
}

func fence(c *queue.Claim) *spend.LeaseFence {
	return &spend.LeaseFence{JobID: c.ID, WorkerID: c.WorkerID, LockedAt: c.LockedAt, LeaseToken: c.LeaseToken}
}

func TestDispatchRequiresLiveLease(t *testing.T) {
	pool, ctx := setup(t, nil, nil)
	c, jobID := claimRunWorkbook(t, pool, "wb1")
	r := res("k1", 10, 100)
	r.RunID = fmt.Sprintf("job:%d", jobID)
	a := reserve(t, pool, r)
	id := ident(a)

	stale := *fence(c)
	stale.LeaseToken++
	id.Lease = &stale
	if transition(t, pool, id, "dispatch") {
		t.Fatal("stale lease token dispatched")
	}
	wrongWorker := *fence(c)
	wrongWorker.WorkerID = "other"
	id.Lease = &wrongWorker
	if transition(t, pool, id, "dispatch") {
		t.Fatal("wrong worker dispatched")
	}
	wrongJob := *fence(c)
	wrongJob.JobID = jobID + 1000
	id.Lease = &wrongJob
	if transition(t, pool, id, "dispatch") {
		t.Fatal("unknown job dispatched")
	}
	// Reservation belongs to another run: run_id predicate must block it.
	other := res("k2", 10, 100)
	other.RowIdentity = "r9"
	other.RunID = "job:999999"
	b := reserve(t, pool, other)
	bid := ident(b)
	bid.Lease = fence(c)
	if transition(t, pool, bid, "dispatch") {
		t.Fatal("dispatch for another run's attempt")
	}
	// Workbook mismatch in job payload.
	if _, err := pool.Exec(ctx, `UPDATE jobs SET payload = '{"workbook_id":"zzz"}' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	id.Lease = fence(c)
	if transition(t, pool, id, "dispatch") {
		t.Fatal("payload workbook mismatch dispatched")
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET payload = '{"workbook_id":"wb1"}' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if !transition(t, pool, id, "dispatch") {
		t.Fatal("live lease must dispatch")
	}
	// Lease lost (status no longer processing) -> release fence for a new attempt fails closed.
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='pending' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	r2 := res("k3", 10, 100)
	r2.RunID, r2.RowIdentity = r.RunID, "r3"
	d := reserve(t, pool, r2)
	did := ident(d)
	did.Lease = fence(c)
	if transition(t, pool, did, "dispatch") {
		t.Fatal("non-processing job dispatched")
	}
	// No lease supplied: plain guarded transition still works (legacy direct caller).
	did.Lease = nil
	if !transition(t, pool, did, "dispatch") {
		t.Fatal("leaseless dispatch")
	}
}

func TestSettleExactlyOnce(t *testing.T) {
	pool, _ := setup(t, f(1), f(0))
	a := reserve(t, pool, res("k1", 100, 1000))
	id := ident(a)
	if s := settle(t, pool, id, 100, "catalog_estimate", map[string]any{"ok": true}); s.OK || s.Reason != "invalid_state" {
		t.Fatalf("settle before dispatch: %+v", s)
	}
	transition(t, pool, id, "dispatch")
	result := map[string]any{"ok": true, "n": 1.0, "name": "José"}
	if s := settle(t, pool, id, 150, "vendor_confirmed", result); !s.OK || s.Reused {
		t.Fatalf("%+v", s)
	}
	if got := spent(t, pool); got != 150.0/1000000 {
		t.Fatalf("budget_spent_usd = %v", got)
	}
	if s := settle(t, pool, id, 150, "vendor_confirmed", result); !s.OK || !s.Reused {
		t.Fatalf("same amount must be reused: %+v", s)
	}
	if got := spent(t, pool); got != 150.0/1000000 {
		t.Fatalf("double charged: %v", got)
	}
	if s := settle(t, pool, id, 151, "vendor_confirmed", result); s.OK || s.Reason != "settlement_conflict" {
		t.Fatalf("%+v", s)
	}
	if s := settle(t, pool, id, 150, "catalog_estimate", result); s.OK || s.Reason != "settlement_conflict" {
		t.Fatalf("basis change: %+v", s)
	}
	if s := settle(t, pool, id, 150, "vendor_confirmed", map[string]any{"ok": false}); s.OK || s.Reason != "settlement_conflict" {
		t.Fatalf("result change: %+v", s)
	}
	bad := id
	bad.ContractHash = "deadbeef"
	if s := settle(t, pool, bad, 150, "vendor_confirmed", result); s.OK || s.Reason != "attempt_not_found" {
		t.Fatalf("%+v", s)
	}
	// Settled attempt cannot be released or marked uncertain.
	if transition(t, pool, id, "release_before_dispatch") || transition(t, pool, id, "mark_uncertain") {
		t.Fatal("settled attempt transitioned")
	}
	// Reserve with the same key now reports the settled status.
	if r := reserve(t, pool, res("k1", 100, 1000)); !r.Reused || r.Status != "settled" {
		t.Fatalf("%+v", r)
	}
}

func TestSettleValidationAndConcurrency(t *testing.T) {
	pool, ctx := setup(t, f(1), f(0))
	a := reserve(t, pool, res("k1", 100, 1000))
	id := ident(a)
	transition(t, pool, id, "dispatch")
	for name, c := range map[string]struct {
		amt   int64
		basis string
	}{"neg": {-1, "catalog_estimate"}, "basis": {1, "guess"}, "nonbillable": {1, "confirmed_nonbillable"}} {
		err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
			_, err := spend.Settle(ctx, tx, id, c.amt, c.basis, map[string]any{})
			return err
		})
		if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	var wg sync.WaitGroup
	var fresh atomic.Int64
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s := settle(t, pool, id, 70, "catalog_estimate", map[string]any{"a": 1.0}); s.OK && !s.Reused {
				fresh.Add(1)
			}
		}()
	}
	wg.Wait()
	if fresh.Load() != 1 || spent(t, pool) != 70.0/1000000 {
		t.Fatalf("fresh=%d spent=%v", fresh.Load(), spent(t, pool))
	}
}

func TestUncertainCannotBeReleased(t *testing.T) {
	pool, _ := setup(t, f(0.0001), nil)
	a := reserve(t, pool, res("k1", 100, 1000))
	id := ident(a)
	if transition(t, pool, id, "mark_uncertain") {
		t.Fatal("reserved -> uncertain must fail")
	}
	if !transition(t, pool, id, "dispatch") || transition(t, pool, id, "dispatch") {
		t.Fatal("dispatch must succeed exactly once")
	}
	if transition(t, pool, id, "release_before_dispatch") {
		t.Fatal("dispatched attempt released")
	}
	if !transition(t, pool, id, "mark_uncertain") {
		t.Fatal("dispatched -> uncertain")
	}
	if transition(t, pool, id, "release_before_dispatch") || transition(t, pool, id, "dispatch") {
		t.Fatal("uncertain attempt must be terminal for release/dispatch")
	}
	if s := settle(t, pool, id, 1, "catalog_estimate", map[string]any{}); s.OK || s.Reason != "invalid_state" {
		t.Fatalf("uncertain settle needs audited path: %+v", s)
	}
	// Uncertain exposure keeps counting against the workbook cap.
	if b := reserve(t, pool, res("k2", 1, 1000)); b.OK || b.Reason != "workbook_budget" {
		t.Fatalf("%+v", b)
	}
}

func TestTransitionUnsupportedAction(t *testing.T) {
	pool, ctx := setup(t, nil, nil)
	a := reserve(t, pool, res("k1", 1, 10))
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		_, err := spend.Transition(ctx, tx, ident(a), "explode")
		return err
	})
	if err == nil {
		t.Fatal("unsupported action accepted")
	}
}
