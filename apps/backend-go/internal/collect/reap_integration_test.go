//go:build integration

package collect

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

// deadCollect enqueues a collect job, claims it for a worker that then died mid-run (the ledger row is running, the
// heartbeat 10 minutes old) and returns the queue row id.
func deadCollect(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string, retries int) int64 {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"job_id": id, "query": "q", "intent": "market_search",
		"workspace_id": itWS, "slug": "s"})
	itJob(t, ctx, pool, itWS, id, "q")
	var qid int64
	if err := pool.QueryRow(ctx, `SELECT id FROM jobs WHERE type = 'collect' AND workspace_id = $1 AND payload::jsonb @> $2::jsonb`,
		itWS, string(payload)).Scan(&qid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'processing', worker_id = 'go-dead-1', retry_count = $2, max_retries = 3,
		locked_at = (now() AT TIME ZONE 'UTC') - interval '20 minutes', lease_token = lease_token + 1,
		last_heartbeat = (now() AT TIME ZONE 'UTC') - interval '10 minutes' WHERE id = $1`, qid, retries); err != nil {
		t.Fatal(err)
	}
	if err := (PgLedger{pool, itWS}).MarkRunning(ctx, id); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	return qid
}

func queueStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, qid int64) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, qid).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// C1/I5: a collect job whose worker died is recovered by the reaper, and the collect failure hook moves its ledger
// row out of running: back to pending while queue retries remain (so the redelivery runs again instead of
// no-op'ing on a running row), failed once they are spent.
func TestReaperReconcilesTheCollectLedger(t *testing.T) {
	pool, ctx := itSetup(t)
	RegisterFailureHook(pool)
	t.Cleanup(func() { queue.RegisterFailureHook(JobTypeCollect, nil) })
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	retry := deadCollect(t, ctx, pool, "jreap-retry", 0)
	final := deadCollect(t, ctx, pool, "jreap-final", 3)
	if _, err := queue.ReapStale(ctx, pool, quiet, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if s := queueStatus(t, ctx, pool, retry); s != "pending" {
		t.Fatalf("retryable queue row %s", s)
	}
	if j, _ := readJob(t, ctx, pool, itWS, "jreap-retry"); j.Status != "pending" || j.Error != "Queue retry scheduled: Heartbeat Timeout" {
		t.Fatalf("retryable ledger row %+v", j)
	}
	if s := queueStatus(t, ctx, pool, final); s != "failed" {
		t.Fatalf("exhausted queue row %s", s)
	}
	j, _ := readJob(t, ctx, pool, itWS, "jreap-final")
	if j.Status != "failed" || j.Error != "Final failure: Heartbeat Timeout" || j.CompletedAt == "" {
		t.Fatalf("exhausted ledger row %+v", j)
	}
}
