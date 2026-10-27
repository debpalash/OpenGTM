//go:build integration

package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const wsAuto = "ws-p08t16"

func autoCfg(on bool) config.Config {
	cfg := e2eConfig()
	cfg.Executor.Workspaces[wsAuto] = true
	cfg.Flags.AutomationsEnabled = on
	cfg.Flags.IntentPollerEnabled = on
	cfg.Flags.PGLeadStore = true
	cfg.Flags.Automations.GoExecutorTriggerEval = on
	cfg.Flags.Automations.GoExecutorWatchPoll = on
	cfg.Flags.Automations.MaxRowsPerEval = 500
	cfg.Flags.Automations.PollerDefaultInterval = "daily"
	cfg.Flags.Automations.PollerMaxConsecutiveFailures = 12
	return cfg
}

func autoSetup(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	return pool, ctx
}

func enqueueGo(t *testing.T, ctx context.Context, pool *pgxpool.Pool, typ, payload string, maxRetries int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (type, payload, status, priority, created_at, retry_count, max_retries, workspace_id, executor)
		VALUES ($1,$2::json,'pending',0,now() AT TIME ZONE 'UTC',0,$3,$4,'go')`, typ, payload, maxRetries, wsAuto); err != nil {
		t.Fatal(err)
	}
}

func jobState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, typ string) (status, executor string) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT status, executor FROM jobs WHERE type=$1 ORDER BY id LIMIT 1`, typ).Scan(&status, &executor); err != nil {
		t.Fatal(err)
	}
	return
}

const evalPayload = `{"workspace_id":"` + wsAuto + `","trigger_id":"nope","fire_key":"p08t16:eval"}`
const pollPayload = `{"workspace_id":"` + wsAuto + `","watch_id":"p08t16-none","fire_key":"p08t16:poll"}`

func TestWorkerRegistersAutomationHandlers(t *testing.T) {
	pool, ctx := autoSetup(t)
	enqueueGo(t, ctx, pool, "trigger_eval", evalPayload, 3)
	enqueueGo(t, ctx, pool, "watch_poll", pollPayload, 3)
	runOne(t, ctx, pool, autoCfg(true))
	for _, typ := range []string{"trigger_eval", "watch_poll"} {
		if st, ex := jobState(t, ctx, pool, typ); st != "completed" || ex != "go" {
			t.Fatalf("%s: status %s executor %s (registered handlers complete a no-op job on Go)", typ, st, ex)
		}
	}
}

// With AUTOMATIONS_ENABLED and the poller off the handlers are still registered (the retired GO_EXECUTOR_* flags
// are ignored): the jobs are no-ops on Go, never handed back.
func TestHandlersRegisteredWithAutomationsOff(t *testing.T) {
	pool, ctx := autoSetup(t)
	cfg := autoCfg(false)
	rn, closeFn, err := buildRunner(cfg, pool, telemetry.NewLogger(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	for typ, payload := range map[string]string{"trigger_eval": evalPayload, "watch_poll": pollPayload} {
		enqueueGo(t, ctx, pool, typ, payload, 3)
		c, err := claimNext(ctx, pool, cfg, "w")
		if err != nil || c == nil || c.Type != typ {
			t.Fatalf("%s: claim %v err %v", typ, c, err)
		}
		if err := rn(ctx, c); err != nil {
			t.Fatalf("%s: err %v, want a no-op", typ, err)
		}
		if _, ex := jobState(t, ctx, pool, typ); ex != "go" {
			t.Fatalf("%s: executor %s", typ, ex)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM jobs`); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFailureHookRegisteredForWatchPoll(t *testing.T) {
	pool, ctx := autoSetup(t)
	cfg := autoCfg(true)
	_, closeFn, err := buildRunner(cfg, pool, telemetry.NewLogger(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	const wid = "p08t16-w"
	if err := tenancy.InWorkspace(ctx, pool, wsAuto, func(tx pgx.Tx) error {
		_, _ = tx.Exec(ctx, `DELETE FROM watch_subscriptions`)
		_, err := tx.Exec(ctx, `INSERT INTO watch_subscriptions (id, workspace_id, kind, target, "interval", enabled, cursor)
			VALUES ($1,$2,'funding','Acme','daily',true,'{"bootstrapped": false}'::json)`, wid, wsAuto)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	enqueueGo(t, ctx, pool, "watch_poll", `{"workspace_id":"`+wsAuto+`","watch_id":"`+wid+`","fire_key":"p08t16:fail"}`, 0)
	c, err := claimNext(ctx, pool, cfg, "w")
	if err != nil || c == nil {
		t.Fatalf("claim %v %v", c, err)
	}
	if _, err := queue.FinalizeAndNotify(ctx, pool, telemetry.NewLogger(io.Discard), c, queue.Outcome{Status: "failed", Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	var fails int
	var lastErr *string
	if err := tenancy.InWorkspace(ctx, pool, wsAuto, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(consecutive_failures,0), last_error FROM watch_subscriptions WHERE id=$1`, wid).Scan(&fails, &lastErr)
	}); err != nil {
		t.Fatal(err)
	}
	if fails != 1 || lastErr == nil {
		t.Fatalf("failure hook did not reconcile: failures=%d last_error=%v", fails, lastErr)
	}
}

// The lease is heartbeated by the job watcher for every handler, including a long single-source
// watch_poll: a handler blocked past several heartbeat intervals keeps its lease fresh.
func TestAutomationJobHeartbeatsWhileHandlerBlocks(t *testing.T) {
	pool, ctx := autoSetup(t)
	t.Setenv("GO_WORKER_TEST_BLOCK_SECONDS", "1")
	cfg := autoCfg(true)
	cfg.HeartbeatInterval = 100 * time.Millisecond
	log := telemetry.NewLogger(io.Discard)
	rn, closeFn, err := buildRunner(cfg, pool, log)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	enqueueGo(t, ctx, pool, "watch_poll", pollPayload, 3)
	c, err := claimNext(ctx, pool, cfg, "w")
	if err != nil || c == nil {
		t.Fatalf("claim %v %v", c, err)
	}
	var first time.Time
	_ = pool.QueryRow(ctx, `SELECT COALESCE(last_heartbeat, locked_at) FROM jobs WHERE id=$1`, c.ID).Scan(&first)
	processJob(ctx, pool, cfg, log, c, rn)
	var last time.Time
	var status string
	if err := pool.QueryRow(ctx, `SELECT last_heartbeat, status FROM jobs WHERE id=$1`, c.ID).Scan(&last, &status); err != nil {
		t.Fatal(err)
	}
	if !last.After(first.Add(300*time.Millisecond)) || status != "completed" {
		t.Fatalf("heartbeat did not advance during the handler: first=%v last=%v status=%s", first, last, status)
	}
}

// A python-executor row is never claimed by the Go worker, flags on or not.
func TestPythonExecutorRowNotClaimedWithFlagOn(t *testing.T) {
	pool, ctx := autoSetup(t)
	cfg := autoCfg(true)
	_, closeFn, err := buildRunner(cfg, pool, telemetry.NewLogger(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	for _, typ := range []string{"trigger_eval", "watch_poll"} {
		if _, err := pool.Exec(ctx, `INSERT INTO jobs (type, payload, status, priority, created_at, retry_count, max_retries, workspace_id, executor)
			VALUES ($1,'{}','pending',0,now() AT TIME ZONE 'UTC',0,3,$2,'python')`, typ, wsAuto); err != nil {
			t.Fatal(err)
		}
	}
	if c, err := claimNext(ctx, pool, cfg, "w"); err != nil || c != nil {
		t.Fatalf("claimed %v err %v", c, err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE status='pending' AND executor='python'`).Scan(&n)
	if n != 2 {
		t.Fatalf("python rows touched: %d pending", n)
	}
}
