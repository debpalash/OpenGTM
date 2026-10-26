//go:build integration

package queue_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

type enqPayload struct {
	WorkspaceID string `json:"workspace_id"`
	Zeta        string `json:"zeta"`
	Alpha       string `json:"alpha"`
}

func TestEnqueueRowShape(t *testing.T) {
	pool, ctx := setup(t)
	before := time.Now().UTC().Add(-2 * time.Second)
	id, err := queue.Enqueue(ctx, pool, "bulk_enrich", "ws-1", enqPayload{"ws-1", "<z>&", "a"}, "fk-1")
	if err != nil || id == 0 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	var typ, status, executor, ws, fk, payload string
	var prio, retries, maxRetries int
	var created, next time.Time
	err = pool.QueryRow(ctx, `SELECT type, status, executor, workspace_id, fire_key, payload::text, priority,
	  retry_count, max_retries, created_at, next_run_at FROM jobs WHERE id=$1`, id).
		Scan(&typ, &status, &executor, &ws, &fk, &payload, &prio, &retries, &maxRetries, &created, &next)
	if err != nil {
		t.Fatal(err)
	}
	if typ != "bulk_enrich" || status != "pending" || executor != "python" || ws != "ws-1" || fk != "fk-1" ||
		prio != 1 || retries != 0 || maxRetries != 3 {
		t.Fatalf("row = %s %s %s %s %s %d %d %d", typ, status, executor, ws, fk, prio, retries, maxRetries)
	}
	if created.Before(before) || created.After(time.Now().UTC().Add(2*time.Second)) || next.Before(before) {
		t.Fatalf("timestamps not naive UTC now: %v %v", created, next)
	}
	// key order preserved, HTML not escaped
	if strings.Index(payload, "zeta") > strings.Index(payload, "alpha") || !strings.Contains(payload, "<z>&") {
		t.Fatalf("payload = %s", payload)
	}
	// empty fire key stores NULL
	id2, err := queue.Enqueue(ctx, pool, "signal_scan", "ws-1", map[string]any{"workspace_id": "ws-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var isNull bool
	if err := pool.QueryRow(ctx, `SELECT fire_key IS NULL FROM jobs WHERE id=$1`, id2).Scan(&isNull); err != nil || !isNull {
		t.Fatalf("fire_key null=%v err=%v", isNull, err)
	}
}

func TestEnqueueRollsBackWithTx(t *testing.T) {
	pool, ctx := setup(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(ctx, tx, "bulk_enrich", "ws-1", map[string]any{"a": 1}, ""); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("count=%d err=%v", n, err)
	}
}
