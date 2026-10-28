//go:build integration

package queue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

// staleClaim claims a fresh row and ages its heartbeat by `age` (a dead worker's lease).
func staleClaim(t *testing.T, pool *pgxpool.Pool, retry, max int, age string) (*queue.Claim, int64) {
	t.Helper()
	c, id := claimed(t, pool, retry, max)
	q := `UPDATE jobs SET last_heartbeat = (now() AT TIME ZONE 'UTC') - interval '` + age + `' WHERE id = $1`
	if age == "null" {
		q = `UPDATE jobs SET last_heartbeat = NULL WHERE id = $1`
	}
	if _, err := pool.Exec(context.Background(), q, id); err != nil {
		t.Fatal(err)
	}
	return c, id
}

type jobRow struct {
	Status, Err         string
	Retry               int
	WorkerNull, LockNil bool
	NextInFuture        bool
	Completed           bool
}

func readJob(t *testing.T, pool *pgxpool.Pool, id int64) jobRow {
	t.Helper()
	var r jobRow
	err := pool.QueryRow(context.Background(), `SELECT status, coalesce(error,''), retry_count, worker_id IS NULL, locked_at IS NULL,
	  coalesce(next_run_at > (now() AT TIME ZONE 'UTC'), false), completed_at IS NOT NULL FROM jobs WHERE id = $1`, id).
		Scan(&r.Status, &r.Err, &r.Retry, &r.WorkerNull, &r.LockNil, &r.NextInFuture, &r.Completed)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A claim whose heartbeat stopped is requeued with the normal backoff while retries remain, and the failure hook
// (which mirrors the failure into domain ledgers) hears about it.
func TestReapStaleRequeuesDeadClaim(t *testing.T) {
	pool, ctx := setup(t)
	r := hookFor(t, nil)
	c, id := staleClaim(t, pool, 0, 3, "10 minutes")
	fresh, freshID := claimed(t, pool, 0, 3)
	_ = fresh
	n, err := queue.ReapStale(ctx, pool, quiet, 5*time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("reaped %d, %v", n, err)
	}
	got := readJob(t, pool, id)
	if got.Status != "pending" || got.Retry != 1 || !got.WorkerNull || !got.LockNil || !got.NextInFuture || got.Err != "Retry 1: Heartbeat Timeout" {
		t.Fatalf("requeued row %+v", got)
	}
	if f := readJob(t, pool, freshID); f.Status != "processing" {
		t.Fatalf("a live claim was reaped: %+v", f)
	}
	if len(r.ns) != 1 || r.ns[0].JobID != id || !r.ns[0].WillRetry || r.ns[0].Reason != "Heartbeat Timeout" || r.ns[0].WorkspaceID != "ws-1" {
		t.Fatalf("hook notices %+v", r.ns)
	}
	// The dead worker coming back is fenced: it cannot overwrite the recovered row.
	if persisted, err := queue.Finalize(ctx, pool, c, queue.Outcome{Status: "completed"}); err != nil || persisted {
		t.Fatalf("stale finalize persisted=%v err=%v", persisted, err)
	}
	if got := readJob(t, pool, id); got.Status != "pending" {
		t.Fatalf("stale finalize changed the row: %+v", got)
	}
}

func TestReapStaleFailsWhenRetriesAreExhausted(t *testing.T) {
	pool, ctx := setup(t)
	r := hookFor(t, nil)
	_, id := staleClaim(t, pool, 3, 3, "null") // never heartbeat at all
	n, err := queue.ReapStale(ctx, pool, quiet, 5*time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("reaped %d, %v", n, err)
	}
	got := readJob(t, pool, id)
	if got.Status != "failed" || got.Retry != 3 || !got.Completed || !got.WorkerNull || got.Err != "Final Failure: Heartbeat Timeout" {
		t.Fatalf("failed row %+v", got)
	}
	if len(r.ns) != 1 || r.ns[0].WillRetry {
		t.Fatalf("hook notices %+v", r.ns)
	}
}

// A failing hook never undoes the queue transition, and a second pass finds nothing left to recover.
func TestReapStaleHookFailureKeepsTheTransition(t *testing.T) {
	pool, ctx := setup(t)
	hookFor(t, errors.New("ledger down"))
	_, id := staleClaim(t, pool, 0, 3, "1 hour")
	if n, err := queue.ReapStale(ctx, pool, quiet, 5*time.Minute); err != nil || n != 1 {
		t.Fatalf("reaped %d, %v", n, err)
	}
	if got := readJob(t, pool, id); got.Status != "pending" {
		t.Fatalf("row %+v", got)
	}
	if n, err := queue.ReapStale(ctx, pool, quiet, 5*time.Minute); err != nil || n != 0 {
		t.Fatalf("second pass reaped %d, %v", n, err)
	}
}

func TestReapStaleLeavesRecentHeartbeats(t *testing.T) {
	pool, ctx := setup(t)
	_, id := staleClaim(t, pool, 0, 3, "2 minutes")
	if n, err := queue.ReapStale(ctx, pool, quiet, 5*time.Minute); err != nil || n != 0 {
		t.Fatalf("reaped %d, %v", n, err)
	}
	if got := readJob(t, pool, id); got.Status != "processing" {
		t.Fatalf("row %+v", got)
	}
}
