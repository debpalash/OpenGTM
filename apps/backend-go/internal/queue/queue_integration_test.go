//go:build integration

package queue_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func setup(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	return pool, ctx
}

func insert(t *testing.T, pool *pgxpool.Pool, typ, executor, ws, status string, retry, maxRetry int) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `INSERT INTO jobs
	 (type, payload, status, priority, created_at, retry_count, max_retries, workspace_id, executor)
	 VALUES ($1, '{}', $2, 0, now() AT TIME ZONE 'UTC', $3, $4, NULLIF($5,''), $6) RETURNING id`,
		typ, status, retry, maxRetry, ws, executor).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustClaim(t *testing.T, pool *pgxpool.Pool, wid string, cap int) *queue.Claim {
	t.Helper()
	c, err := queue.ClaimNext(context.Background(), pool, wid, cap)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("expected a claim")
	}
	return c
}

func TestGoClaimsOnlyGoRows(t *testing.T) {
	pool, ctx := setup(t)
	insert(t, pool, "run_workbook", "python", "a", "pending", 0, 3)
	goID := insert(t, pool, "run_workbook", "go", "a", "pending", 0, 3)
	c := mustClaim(t, pool, "w1", 0)
	if c.ID != goID || c.LeaseToken != 1 || c.WorkerID != "w1" {
		t.Fatalf("claim = %+v", c)
	}
	if c2, _ := queue.ClaimNext(ctx, pool, "w1", 0); c2 != nil {
		t.Fatalf("python row claimed: %+v", c2)
	}
}

func TestTenantCapRespected(t *testing.T) {
	pool, _ := setup(t)
	var a []int64
	for i := 0; i < 5; i++ {
		a = append(a, insert(t, pool, "run_workbook", "go", "A", "pending", 0, 3))
	}
	b := insert(t, pool, "run_workbook", "go", "B", "pending", 0, 3)
	c1 := mustClaim(t, pool, "w", 2)
	c2 := mustClaim(t, pool, "w", 2)
	c3 := mustClaim(t, pool, "w", 2)
	if c1.ID != a[0] || c2.ID != b || c3.ID != a[1] {
		t.Fatalf("order: %d %d %d (a=%v b=%d)", c1.ID, c2.ID, c3.ID, a, b)
	}
	// A is now at cap 2 active: no further A claim; B has none left.
	if c4, _ := queue.ClaimNext(context.Background(), pool, "w", 2); c4 != nil {
		t.Fatalf("cap exceeded: %+v", c4)
	}
}

func TestFinalizePreservesCancelled(t *testing.T) {
	pool, ctx := setup(t)
	id := insert(t, pool, "run_workbook", "go", "a", "pending", 0, 3)
	c := mustClaim(t, pool, "w", 0)
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if cancelled, err := queue.IsCancelled(ctx, pool, c); err != nil || !cancelled {
		t.Fatalf("IsCancelled = %v, %v", cancelled, err)
	}
	if _, err := queue.Finalize(ctx, pool, c, queue.Outcome{Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	var st string
	var wid *string
	var done *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, worker_id, completed_at FROM jobs WHERE id=$1`, id).Scan(&st, &wid, &done); err != nil {
		t.Fatal(err)
	}
	if st != "cancelled" || wid != nil || done == nil {
		t.Fatalf("status=%s worker=%v completed=%v", st, wid, done)
	}
}

func TestFinalizeRetryBackoff(t *testing.T) {
	pool, ctx := setup(t)
	id := insert(t, pool, "run_workbook", "go", "a", "pending", 0, 2)
	for want, backoff := range []int{1, 2} {
		c := mustClaim(t, pool, "w", 0)
		persisted, err := queue.Finalize(ctx, pool, c, queue.Outcome{Status: "failed", Error: "boom"})
		if err != nil || !persisted {
			t.Fatalf("finalize: %v %v", persisted, err)
		}
		var st, e string
		var rc int
		var mins float64
		if err := pool.QueryRow(ctx, `SELECT status, retry_count, error,
		  EXTRACT(EPOCH FROM (next_run_at - (now() AT TIME ZONE 'UTC')))/60 FROM jobs WHERE id=$1`, id).Scan(&st, &rc, &e, &mins); err != nil {
			t.Fatal(err)
		}
		if st != "pending" || rc != want+1 || mins < float64(backoff)-0.5 || mins > float64(backoff) {
			t.Fatalf("attempt %d: %s rc=%d mins=%.2f", want, st, rc, mins)
		}
		if e != "Retry "+string(rune('1'+want))+": boom" {
			t.Fatalf("error %q", e)
		}
		// Make it due again.
		if _, err := pool.Exec(ctx, `UPDATE jobs SET next_run_at = NULL WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	c := mustClaim(t, pool, "w", 0)
	if _, err := queue.Finalize(ctx, pool, c, queue.Outcome{Status: "failed", Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	var st, e string
	var wid *string
	pool.QueryRow(ctx, `SELECT status, error, worker_id FROM jobs WHERE id=$1`, id).Scan(&st, &e, &wid)
	if st != "failed" || e != "Final Failure: boom" || wid != nil {
		t.Fatalf("final: %s %q %v", st, e, wid)
	}
}

func TestFinalizeAfterLeaseLossIsNoop(t *testing.T) {
	pool, ctx := setup(t)
	id := insert(t, pool, "run_workbook", "go", "a", "pending", 0, 3)
	c := mustClaim(t, pool, "w", 0)
	// Reaped then reclaimed by someone else: token advances.
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='pending', worker_id=NULL, locked_at=NULL, next_run_at=NULL WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	c2 := mustClaim(t, pool, "w2", 0)
	if c2.LeaseToken != c.LeaseToken+1 {
		t.Fatalf("token %d -> %d", c.LeaseToken, c2.LeaseToken)
	}
	persisted, err := queue.Finalize(ctx, pool, c, queue.Outcome{Status: "completed"})
	if err != nil || persisted {
		t.Fatalf("stale finalize persisted=%v err=%v", persisted, err)
	}
	var st string
	pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, id).Scan(&st)
	if st != "processing" {
		t.Fatalf("status %s", st)
	}
}

func TestHeartbeatDetectsLeaseLoss(t *testing.T) {
	pool, ctx := setup(t)
	id := insert(t, pool, "run_workbook", "go", "a", "pending", 0, 3)
	c := mustClaim(t, pool, "w", 0)
	if ok, err := queue.Heartbeat(ctx, pool, c); err != nil || !ok {
		t.Fatalf("heartbeat %v %v", ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET lease_token = lease_token + 1 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if ok, err := queue.Heartbeat(ctx, pool, c); err != nil || ok {
		t.Fatalf("expected lease loss, got %v %v", ok, err)
	}
}

func TestSchemaGateRefusesOldRevision(t *testing.T) {
	pool, ctx := setup(t)
	var orig string
	if err := pool.QueryRow(ctx, `SELECT version_num FROM alembic_version`).Scan(&orig); err != nil {
		t.Fatal(err)
	}
	if orig != config.RequiredGoSchemaRevision {
		t.Fatalf("test db at %s, want %s", orig, config.RequiredGoSchemaRevision)
	}
	if err := queue.CheckSchema(ctx, pool, config.RequiredGoSchemaRevision); err != nil {
		t.Fatal(err)
	}
	if err := queue.CheckSchema(ctx, pool, "000000000000"); err == nil {
		t.Fatal("expected mismatch error")
	}
}

func TestUnsupportedGoJobTypeFails(t *testing.T) {
	pool, ctx := setup(t)
	bad := insert(t, pool, "enrich_lead", "go", "a", "pending", 0, 3)
	good := insert(t, pool, "run_workbook", "go", "a", "pending", 0, 3)
	c := mustClaim(t, pool, "w", 0)
	if c.ID != good {
		t.Fatalf("claimed %d, want %d", c.ID, good)
	}
	var st, e string
	var wid *string
	pool.QueryRow(ctx, `SELECT status, error, worker_id FROM jobs WHERE id=$1`, bad).Scan(&st, &e, &wid)
	if st != "failed" || e != "go executor: unsupported job type" || wid != nil {
		t.Fatalf("%s %q %v", st, e, wid)
	}
}
