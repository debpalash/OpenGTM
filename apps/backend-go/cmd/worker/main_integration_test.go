//go:build integration

package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
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

func e2eConfig() config.Config {
	return config.Config{
		HeartbeatInterval: time.Hour, JobTimeout: time.Minute, AppEnv: "test", MaxActivePerWorkspace: 0,
		InternalServiceToken: "t", CredentialsSource: config.CredentialsSourceHTTP,
		Executor: config.ExecutorSettings{Enabled: true, SignalScan: true, Workspaces: map[string]bool{"wsr": true},
			SignalHotLeads: 50, SignalWarmLeads: 30, SignalPerWorkspace: 20, SignalGlobal: 500},
	}
}

// runOne runs the real runner (buildRunner) over a slot until no pending go
// job is left.
func runOne(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cfg config.Config) {
	t.Helper()
	log := telemetry.NewLogger(io.Discard)
	rn, closeFn, err := buildRunner(cfg, pool, log)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	cctx, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	done := make(chan struct{})
	go func() { slotLoop(cctx, ctx, pool, cfg, log, "w", rn); close(done) }()
	for cctx.Err() == nil {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE executor='go' AND status IN ('pending','processing') AND (next_run_at IS NULL OR next_run_at <= now() AT TIME ZONE 'UTC')`).Scan(&n)
		if n == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	stop()
	<-done
}

func TestRefreshJobEndToEnd(t *testing.T) {
	pool := testpg.NewAppPool(t, 8)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	ws := "wsr"
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO workbooks (id, name, workspace_id, columns_config, refresh_policy)
			VALUES ('wbr','wbr',$1,'[{"id":"email","type":"enrichment"}]'::json,'{"enabled":true,"interval":"daily"}'::json)`, ws); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO workbook_rows (id, workbook_id, workspace_id, data) VALUES (9001,'wbr',$1,'{}'::json)`, ws)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"workbook_id": "wbr", "workspace_id": ws})
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (type, payload, status, priority, created_at, retry_count, max_retries, workspace_id, executor)
		VALUES ('refresh_workbook',$1,'pending',0,now() AT TIME ZONE 'UTC',0,3,$2,'go')`, string(payload), ws); err != nil {
		t.Fatal(err)
	}
	runOne(t, ctx, pool, e2eConfig())
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE type='refresh_workbook' ORDER BY id LIMIT 1`).Scan(&status); err != nil || status != "completed" {
		t.Fatalf("refresh job status %q err %v", status, err)
	}
	var runs int
	var nextExec string
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE type='run_workbook'`).Scan(&runs)
	_ = pool.QueryRow(ctx, `SELECT executor FROM jobs WHERE type='refresh_workbook' AND status='pending'`).Scan(&nextExec)
	if runs != 1 || nextExec != "go" {
		t.Fatalf("run jobs %d, next refresh executor %q", runs, nextExec)
	}
}

func TestSignalScanJobEndToEnd(t *testing.T) {
	pool := testpg.NewAppPool(t, 8)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (type, payload, status, priority, created_at, retry_count, max_retries, workspace_id, executor)
		VALUES ('signal_scan','{"interval_minutes":60}','pending',0,now() AT TIME ZONE 'UTC',0,3,NULL,'go')`); err != nil {
		t.Fatal(err)
	}
	runOne(t, ctx, pool, e2eConfig())
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE type='signal_scan' ORDER BY id LIMIT 1`).Scan(&status); err != nil || status != "completed" {
		t.Fatalf("scan job status %q err %v", status, err)
	}
	var next int
	var ex string
	_ = pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(executor),'') FROM jobs WHERE type='signal_scan' AND status='pending'`).Scan(&next, &ex)
	if next != 1 || ex != "go" {
		t.Fatalf("next scan: %d executor %q", next, ex)
	}
}

// Only registered types are claimed; an unknown go job is failed by the claim
// itself and the runner is never invoked.
func TestUnknownTypeNotClaimed(t *testing.T) {
	pool := testpg.NewAppPool(t, 8)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (type, payload, status, priority, created_at, retry_count, max_retries, workspace_id, executor)
		VALUES ('mystery_type','{}','pending',0,now() AT TIME ZONE 'UTC',0,3,'ws','go')`); err != nil {
		t.Fatal(err)
	}
	cfg := e2eConfig()
	if _, _, err := buildRunner(cfg, pool, telemetry.NewLogger(io.Discard)); err != nil {
		t.Fatal(err)
	}
	c, err := claimNext(ctx, pool, cfg, "w")
	if err != nil || c != nil {
		t.Fatalf("claimed %v err %v", c, err)
	}
	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM jobs WHERE type='mystery_type'`).Scan(&status)
	if status == "processing" {
		t.Fatalf("status %s", status)
	}
}
