//go:build integration

package queue_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

func TestEnqueueOnceDedupesConcurrent(t *testing.T) {
	pool, ctx := setup(t)
	var created atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := pool.Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			_, ok, err := queue.EnqueueOnce(ctx, tx, "refresh_workbook", "ws-1", map[string]any{"workbook_id": "w"}, "refresh:w:1", "go")
			if err != nil {
				_ = tx.Rollback(ctx)
				errs <- err
				return
			}
			if err := tx.Commit(ctx); err != nil {
				errs <- err
				return
			}
			if ok {
				created.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE fire_key='refresh:w:1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows=%d err=%v", n, err)
	}
	if created.Load() != 1 {
		t.Fatalf("created=%d, want 1", created.Load())
	}
	// A finished occurrence frees the key.
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='completed' WHERE fire_key='refresh:w:1'`); err != nil {
		t.Fatal(err)
	}
	tx, _ := pool.Begin(ctx)
	_, ok, err := queue.EnqueueOnce(ctx, tx, "refresh_workbook", "ws-1", map[string]any{"workbook_id": "w"}, "refresh:w:1", "go")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	_ = tx.Commit(ctx)
}

func TestEnqueueOnceRowShapeMatchesPython(t *testing.T) {
	pool, ctx := setup(t)
	before := time.Now().UTC().Add(-2 * time.Second)
	tx, _ := pool.Begin(ctx)
	id, ok, err := queue.EnqueueOnce(ctx, tx, "signal_scan", "ws-1", map[string]any{"b": "<x>&", "a": 1}, "scan:1", "go")
	if err != nil || !ok || id == 0 {
		t.Fatalf("id=%d ok=%v err=%v", id, ok, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
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
	if typ != "signal_scan" || status != "pending" || executor != "go" || ws != "ws-1" || fk != "scan:1" ||
		prio != 1 || retries != 0 || maxRetries != 3 {
		t.Fatalf("row = %s %s %s %s %s %d %d %d", typ, status, executor, ws, fk, prio, retries, maxRetries)
	}
	if created.Before(before) || next.Before(before) || created.After(time.Now().UTC().Add(2*time.Second)) {
		t.Fatalf("timestamps not naive UTC now: %v %v", created, next)
	}
	// Python: {**payload, "fire_key": fire_key} -> fire_key last, HTML unescaped.
	if !strings.HasSuffix(payload, `"fire_key": "scan:1"}`) && !strings.HasSuffix(payload, `"fire_key":"scan:1"}`) {
		t.Fatalf("fire_key not last: %s", payload)
	}
	if !strings.Contains(payload, "<x>&") || strings.Index(payload, `"b"`) > strings.Index(payload, `"a"`) {
		t.Fatalf("payload = %s", payload)
	}
	// Executor is explicit.
	tx, _ = pool.Begin(ctx)
	id2, _, err := queue.EnqueueOnce(ctx, tx, "refresh_workbook", "ws-1", map[string]any{}, "k2", "python")
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)
	if err := pool.QueryRow(ctx, `SELECT executor FROM jobs WHERE id=$1`, id2).Scan(&executor); err != nil || executor != "python" {
		t.Fatalf("executor=%q err=%v", executor, err)
	}
	// Invalid input is rejected.
	tx, _ = pool.Begin(ctx)
	defer tx.Rollback(context.Background())
	if _, _, err := queue.EnqueueOnce(ctx, tx, "x", "ws-1", map[string]any{}, "", "go"); err == nil {
		t.Fatal("empty fire key accepted")
	}
	if _, _, err := queue.EnqueueOnce(ctx, tx, "x", "ws-1", map[string]any{}, "k", "bogus"); err == nil {
		t.Fatal("bad executor accepted")
	}
}

func TestEnqueueOnceRollsBackWithTx(t *testing.T) {
	pool, ctx := setup(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := queue.EnqueueOnce(ctx, tx, "refresh_workbook", "ws-1", map[string]any{"a": 1}, "rb:1", "go"); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("count=%d err=%v", n, err)
	}
	// Key is reusable after rollback.
	tx, _ = pool.Begin(ctx)
	if _, ok, err := queue.EnqueueOnce(ctx, tx, "refresh_workbook", "ws-1", map[string]any{"a": 1}, "rb:1", "go"); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	_ = tx.Commit(ctx)
}
