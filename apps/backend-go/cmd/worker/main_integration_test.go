//go:build integration

package main

import (
	"context"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// With MaxActivePerWorkspace=1 and two slots, two jobs of one workspace must
// never run concurrently.
func TestSlotLoopHonoursTenantCap(t *testing.T) {
	pool := testpg.NewAppPool(t, 8)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO jobs (type, payload, status, priority, created_at, retry_count, max_retries, workspace_id, executor)
		  VALUES ('run_workbook','{}','pending',0,now() AT TIME ZONE 'UTC',0,3,'ws','go')`); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{MaxActivePerWorkspace: 1, HeartbeatInterval: time.Hour}
	var running, maxRunning, total atomic.Int32
	rn := func(ctx context.Context, _ *queue.Claim) error {
		n := running.Add(1)
		for {
			m := maxRunning.Load()
			if n <= m || maxRunning.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(700 * time.Millisecond)
		running.Add(-1)
		total.Add(1)
		return nil
	}
	cctx, stop := context.WithTimeout(ctx, 6*time.Second)
	defer stop()
	log := telemetry.NewLogger(io.Discard)
	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() { slotLoop(cctx, ctx, pool, cfg, log, "w", rn); done <- struct{}{} }()
	}
	for total.Load() < 2 && cctx.Err() == nil {
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	<-done
	<-done
	if total.Load() != 2 {
		t.Fatalf("only %d jobs ran", total.Load())
	}
	if maxRunning.Load() != 1 {
		t.Fatalf("max concurrent = %d, want 1", maxRunning.Load())
	}
}

// The pooled connections carry lock_timeout / statement_timeout from poolConfig.
func TestPoolConnectionCarriesTimeouts(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	pc, err := poolConfig(config.Config{DatabaseURL: url, DBMaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var lock, stmt string
	if err := pool.QueryRow(context.Background(), `SHOW lock_timeout`).Scan(&lock); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SHOW statement_timeout`).Scan(&stmt); err != nil {
		t.Fatal(err)
	}
	if lock != "30s" || stmt != "1min" && stmt != "60s" {
		t.Fatalf("lock_timeout=%q statement_timeout=%q", lock, stmt)
	}
}

// A panicking runner fails the job through the normal retry path and the worker
// goes on to claim the next job.
func TestPanickingRunnerFailsJobAndSlotSurvives(t *testing.T) {
	pool := testpg.NewAppPool(t, 8)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO jobs (type, payload, status, priority, created_at, retry_count, max_retries, workspace_id, executor)
		  VALUES ('run_workbook','{}','pending',0,now() AT TIME ZONE 'UTC',0,0,'ws','go')`); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{HeartbeatInterval: time.Hour, JobTimeout: time.Minute}
	var calls atomic.Int32
	rn := func(context.Context, *queue.Claim) error { calls.Add(1); panic("boom") }
	cctx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	done := make(chan struct{})
	go func() { slotLoop(cctx, ctx, pool, cfg, telemetry.NewLogger(io.Discard), "w", rn); close(done) }()
	for calls.Load() < 2 && cctx.Err() == nil {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	stop()
	<-done
	var n int
	var msg string
	if err := pool.QueryRow(ctx, `SELECT count(*), max(error) FROM jobs WHERE status='failed'`).Scan(&n, &msg); err != nil {
		t.Fatal(err)
	}
	if n != 2 || msg != "Final Failure: panic: boom" {
		t.Fatalf("failed=%d error=%q", n, msg)
	}
}
