//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/playbooks"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/retention"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
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
		if _, err := tx.Exec(ctx, `DELETE FROM workbooks WHERE id='wbr'`); err != nil {
			return err
		}
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

// ---- governance handlers (plan 11 task 18) ----

type govEnv struct {
	pool, owner *pgxpool.Pool
	ws          string
}

var govCtr atomic.Int32

func setupGov(t *testing.T) *govEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	owner, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	ws := fmt.Sprintf("wsgov%d-%d", os.Getpid(), govCtr.Add(1))
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id,name,slug,description,icon,created_at,updated_at) VALUES ($1::text,$1::text,$1::text,'','',1,1)`, ws); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = owner.Exec(c, `DELETE FROM jobs WHERE workspace_id=$1`, ws)
		for _, q := range []string{`DELETE FROM signals`, `DELETE FROM retention_runs`, `DELETE FROM retention_policies`, `DELETE FROM retention_schedules`,
			`DELETE FROM playbook_results`, `DELETE FROM playbook_runs`, `DELETE FROM playbook_schedules`, `DELETE FROM audience_members`,
			`DELETE FROM research_playbooks`, `DELETE FROM audiences`} {
			_, _ = owner.Exec(c, q+` WHERE workspace_id=$1`, ws)
		}
		_, _ = owner.Exec(c, `DELETE FROM workspaces WHERE id=$1`, ws)
	})
	return &govEnv{pool: pool, owner: owner, ws: ws}
}

func govConfig(ws string) config.Config {
	return config.Config{
		HeartbeatInterval: time.Hour, JobTimeout: time.Minute, AppEnv: "test",
		Executor: config.ExecutorSettings{Enabled: true, Workspaces: map[string]bool{ws: true}},
		Flags:    config.Flags{Governance: config.GovernanceSettings{GoExecutorRetention: true, GoExecutorPlaybooks: true}},
	}
}

// runGov runs the governance handlers over a slot until no pending go job is due.
func (g *govEnv) runGov(t *testing.T, cfg config.Config, o governanceOpts) {
	t.Helper()
	ctx := context.Background()
	log := telemetry.NewLogger(io.Discard)
	rn := dispatch(governanceHandlers(cfg, g.pool, log, nil, o))
	cctx, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	done := make(chan struct{})
	go func() { slotLoop(cctx, ctx, g.pool, cfg, log, "w", rn); close(done) }()
	for cctx.Err() == nil {
		var n int
		_ = g.pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE executor='go' AND status IN ('pending','processing') AND (next_run_at IS NULL OR next_run_at <= now() AT TIME ZONE 'UTC')`).Scan(&n)
		if n == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	stop()
	<-done
}

var govDays = map[string]int{"audit": 90, "llm_usage": 30, "signals": 30, "activation": 30, "audience_history": 30, "agent_results": 30, "outreach_history": 30}

// retentionRun inserts an enabled policy, a pending run with the given snapshot and a
// go retention_enforce job for it.
func (g *govEnv) retentionRun(t *testing.T, snapshot string, maxRetries int) (runID string, jobID int64) {
	t.Helper()
	ctx := context.Background()
	days, _ := json.Marshal(govDays)
	if _, err := g.owner.Exec(ctx, `INSERT INTO retention_policies (workspace_id,enabled,legal_hold,retention_days) VALUES ($1,true,false,$2)`, g.ws, days); err != nil {
		t.Fatal(err)
	}
	if err := g.owner.QueryRow(ctx, `INSERT INTO retention_runs (id,workspace_id,status,requested_by,policy_snapshot,deleted_counts)
		VALUES (gen_random_uuid()::text,$1,'pending','7',$2::json,'{}') RETURNING id`, g.ws, snapshot).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"workspace_id": g.ws, "run_id": runID})
	if err := g.owner.QueryRow(ctx, `INSERT INTO jobs (type,payload,status,priority,created_at,retry_count,max_retries,workspace_id,executor)
		VALUES ('retention_enforce',$1,'pending',0,now() AT TIME ZONE 'UTC',0,$2,$3,'go') RETURNING id`, string(payload), maxRetries, g.ws).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	return
}

func (g *govEnv) status(t *testing.T, q string, args ...any) (status, errText string) {
	t.Helper()
	var e *string
	if err := g.owner.QueryRow(context.Background(), q, args...).Scan(&status, &e); err != nil {
		t.Fatal(err)
	}
	if e != nil {
		errText = *e
	}
	return
}

func (g *govEnv) runStatus(t *testing.T, id string) (string, string) {
	return g.status(t, `SELECT status, error FROM retention_runs WHERE id=$1`, id)
}

func (g *govEnv) jobStatus(t *testing.T, id int64) (string, string) {
	return g.status(t, `SELECT status, error FROM jobs WHERE id=$1`, id)
}

func snapshotJSON() string { b, _ := json.Marshal(govDays); return string(b) }

func TestWorkerRunsRetentionEnforceJob(t *testing.T) {
	g := setupGov(t)
	run, job := g.retentionRun(t, snapshotJSON(), 3)
	g.runGov(t, govConfig(g.ws), governanceOpts{})
	if s, e := g.runStatus(t, run); s != "completed" {
		t.Fatalf("run %s (%s)", s, e)
	}
	if s, e := g.jobStatus(t, job); s != "completed" {
		t.Fatalf("job %s (%s)", s, e)
	}
}

type govRes struct{ calls atomic.Int32 }

func (r *govRes) Research(context.Context, string, playbooks.ResearchRequest) (playbooks.ResearchResult, error) {
	r.calls.Add(1)
	return playbooks.ResearchResult{Success: true, Value: "v"}, nil
}

// playbookFixture creates an audience with one member and an enabled playbook.
func (g *govEnv) playbookFixture(t *testing.T) (pb playbooks.Playbook, aud string) {
	t.Helper()
	ctx := context.Background()
	aud = g.ws + "-aud"
	err := tenancy.InWorkspace(ctx, g.pool, g.ws, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO audiences (id, workspace_id, name, filters) VALUES ($1,$2,$1,'{}')`, aud, g.ws); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audience_members (workspace_id, audience_id, lead_id, snapshot) VALUES ($1,$2,1,'{"lead":1}'::json)`, g.ws, aud); err != nil {
			return err
		}
		var err error
		pb, err = playbooks.Create(ctx, tx, g.ws, playbooks.CreateInput{Name: "gov-pb", PromptTemplate: "prompt template text", OutputFormat: "text",
			MaxSteps: 4, CellBudgetUSD: 0.1, Steps: []playbooks.Step{{Key: "s1", Name: "S1", PromptTemplate: "step one prompt", OutputFormat: "text"}}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

func (g *govEnv) playbookRun(t *testing.T, pb playbooks.Playbook, aud string) (runID string, jobID int64) {
	t.Helper()
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, g.pool, g.ws, func(tx pgx.Tx) error {
		r, err := playbooks.StartRun(ctx, tx, g.ws, pb, aud, 100, "7")
		runID = r.ID
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"workspace_id": g.ws, "run_id": runID})
	if err := g.owner.QueryRow(ctx, `INSERT INTO jobs (type,payload,status,priority,created_at,retry_count,max_retries,workspace_id,executor)
		VALUES ('research_playbook_run',$1,'pending',0,now() AT TIME ZONE 'UTC',0,3,$2,'go') RETURNING id`, string(payload), g.ws).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	return
}

func (g *govEnv) pbRunStatus(t *testing.T, id string) (string, string) {
	return g.status(t, `SELECT status, error FROM playbook_runs WHERE id=$1`, id)
}

func TestWorkerRunsPlaybookRunJob(t *testing.T) {
	g := setupGov(t)
	pb, aud := g.playbookFixture(t)
	run, job := g.playbookRun(t, pb, aud)
	res := &govRes{}
	g.runGov(t, govConfig(g.ws), governanceOpts{researcher: res})
	if s, e := g.pbRunStatus(t, run); s != "completed" {
		t.Fatalf("run %s (%s)", s, e)
	}
	if s, e := g.jobStatus(t, job); s != "completed" || res.calls.Load() != 1 {
		t.Fatalf("job %s (%s), research calls %d", s, e, res.calls.Load())
	}
}

func TestWorkerRunsPlaybookScheduleJob(t *testing.T) {
	g := setupGov(t)
	pb, aud := g.playbookFixture(t)
	ctx := context.Background()
	if _, err := g.owner.Exec(ctx, `UPDATE research_playbooks SET enabled=true, schedule_audience_id=$3, schedule_interval_minutes=60 WHERE workspace_id=$1 AND id=$2`, g.ws, pb.ID, aud); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"workspace_id": g.ws, "playbook_id": pb.ID})
	var job int64
	if err := g.owner.QueryRow(ctx, `INSERT INTO jobs (type,payload,status,priority,created_at,retry_count,max_retries,workspace_id,executor)
		VALUES ('research_playbook_schedule',$1,'pending',0,now() AT TIME ZONE 'UTC',0,3,$2,'go') RETURNING id`, string(payload), g.ws).Scan(&job); err != nil {
		t.Fatal(err)
	}
	// Only the schedule job runs: the run job it enqueues is for the cohort's executor, and
	// the run is checked, not executed (no researcher, so it would be handed back anyway).
	g.runGov(t, govConfig(g.ws), governanceOpts{researcher: &govRes{}})
	if s, e := g.jobStatus(t, job); s != "completed" {
		t.Fatalf("schedule job %s (%s)", s, e)
	}
	var runs int
	var by string
	if err := g.owner.QueryRow(ctx, `SELECT count(*), COALESCE(max(requested_by),'') FROM playbook_runs WHERE workspace_id=$1 AND playbook_id=$2`, g.ws, pb.ID).Scan(&runs, &by); err != nil || runs != 1 || by != "scheduler" {
		t.Fatalf("runs=%d by=%q err=%v", runs, by, err)
	}
}

// Without a Python worker the run job goes back to Python untouched, no retry spent.
func TestWorkerHandsBackPlaybookRunWithoutPyWorker(t *testing.T) {
	g := setupGov(t)
	pb, aud := g.playbookFixture(t)
	run, job := g.playbookRun(t, pb, aud)
	g.runGov(t, govConfig(g.ws), governanceOpts{})
	var status, executor string
	var retries int
	if err := g.owner.QueryRow(context.Background(), `SELECT status, executor, retry_count FROM jobs WHERE id=$1`, job).Scan(&status, &executor, &retries); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || executor != "python" || retries != 0 {
		t.Fatalf("job %s/%s retries %d", status, executor, retries)
	}
	if s, _ := g.pbRunStatus(t, run); s != "pending" {
		t.Fatalf("run %s, want pending (as found)", s)
	}
}

// A handler error fails the attempt; the queue's failure hook reconciles the run with
// willRetry = retry_count < max_retries read from the job row.
func TestHandlerErrorInvokesReconcileWithRetryFlag(t *testing.T) {
	for _, tc := range []struct {
		name       string
		maxRetries int
		prefix     string
		runStatus  string
	}{{"retry", 3, "Queue retry scheduled:", "pending"}, {"final", 0, "Final failure:", "failed"}} {
		t.Run(tc.name, func(t *testing.T) {
			g := setupGov(t)
			run, job := g.retentionRun(t, `{"audit": 1}`, tc.maxRetries) // snapshot without every category: Enforce fails
			g.runGov(t, govConfig(g.ws), governanceOpts{})
			s, e := g.runStatus(t, run)
			if s != tc.runStatus || !strings.HasPrefix(e, tc.prefix) {
				t.Fatalf("run %s %q, want %s %q...", s, e, tc.runStatus, tc.prefix)
			}
			if js, _ := g.jobStatus(t, job); tc.maxRetries == 0 && js != "failed" {
				t.Fatalf("job %s", js)
			}
		})
	}
}

func TestPanicIsRecoveredAndReconciled(t *testing.T) {
	g := setupGov(t)
	run, job := g.retentionRun(t, snapshotJSON(), 0)
	g.runGov(t, govConfig(g.ws), governanceOpts{afterBatch: func(string, int64) { panic("boom") }})
	s, e := g.runStatus(t, run)
	if s != "failed" || !strings.HasPrefix(e, "Final failure: panic: boom") {
		t.Fatalf("run %s %q", s, e)
	}
	if js, _ := g.jobStatus(t, job); js != "failed" {
		t.Fatalf("job %s", js)
	}
}

// When the lease is lost mid-run the heartbeat cancels the context and the purge stops
// before its next batch.
func TestLeaseLostStopsPurgeBeforeNextBatch(t *testing.T) {
	g := setupGov(t)
	ctx := context.Background()
	old := float64(time.Now().Add(-200*24*time.Hour).UnixMicro()) / 1e6
	for i := 0; i < 3; i++ {
		if _, err := g.owner.Exec(ctx, `INSERT INTO signals (id,workspace_id,created_at) VALUES ($1,$2,$3)`, fmt.Sprintf("%s-s%d", g.ws, i), g.ws, old); err != nil {
			t.Fatal(err)
		}
	}
	_, job := g.retentionRun(t, snapshotJSON(), 3)
	cfg := govConfig(g.ws)
	cfg.HeartbeatInterval = 50 * time.Millisecond
	var batches atomic.Int32
	g.runGov(t, cfg, governanceOpts{batchRows: 1, afterBatch: func(cat string, n int64) {
		if cat != "signals" {
			return
		}
		if batches.Add(1) == 1 {
			_, _ = g.owner.Exec(ctx, `UPDATE jobs SET worker_id='someone-else', status='completed' WHERE id=$1`, job)
			time.Sleep(500 * time.Millisecond) // let a heartbeat notice
		}
	}})
	var left int
	if err := g.owner.QueryRow(ctx, `SELECT count(*) FROM signals WHERE workspace_id=$1`, g.ws).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if batches.Load() != 1 || left != 2 {
		t.Fatalf("batches=%d signals left=%d, want 1 and 2", batches.Load(), left)
	}
}

// Go never claims executor='python' rows; with both flags off every governance type is
// still handled (handed back), so no go row is ever failed as unsupported.
func TestFlagsOffLeavePythonJobsUntouched(t *testing.T) {
	g := setupGov(t)
	ctx := context.Background()
	if h := governanceHandlers(govConfig(g.ws), g.pool, telemetry.NewLogger(io.Discard), nil, governanceOpts{}); len(h) != 3 {
		t.Fatalf("flags on: %d handlers", len(h))
	}
	var job int64
	if err := g.owner.QueryRow(ctx, `INSERT INTO jobs (type,payload,status,priority,created_at,retry_count,max_retries,workspace_id,executor)
		VALUES ('retention_enforce',$1,'pending',0,now() AT TIME ZONE 'UTC',0,3,$2,'python') RETURNING id`, `{"workspace_id":"`+g.ws+`"}`, g.ws).Scan(&job); err != nil {
		t.Fatal(err)
	}
	if c, err := claimNext(ctx, g.pool, govConfig(g.ws), "w"); err != nil || c != nil {
		t.Fatalf("claimed %v err %v", c, err)
	}
	if s, _ := g.jobStatus(t, job); s != "pending" {
		t.Fatalf("python job %s", s)
	}
	off := govConfig(g.ws)
	off.Flags.Governance = config.GovernanceSettings{}
	if h := governanceHandlers(off, g.pool, telemetry.NewLogger(io.Discard), nil, governanceOpts{}); len(h) != 3 {
		t.Fatalf("flags off: %d handlers, want 3 hand-back handlers", len(h))
	}
}

// Python's queue reaper (_reconcile_dead_jobs / reap_dead_jobs_once) selects processing
// jobs by heartbeat and looks the failure handler up by job TYPE, not executor, so a
// Go-claimed job whose lease expired is reconciled by Python's reaper using the same
// registered hooks. The Go worker has no reaper of its own. This test feeds the hook the
// notice that reaper builds for a dead go claim and checks the run is reconciled.
func TestDeadGoLeaseReconciled(t *testing.T) {
	g := setupGov(t)
	run, job := g.retentionRun(t, snapshotJSON(), 3)
	if _, err := g.owner.Exec(context.Background(), `UPDATE retention_runs SET status='running' WHERE id=$1`, run); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if err := g.owner.QueryRow(context.Background(), `SELECT payload::text FROM jobs WHERE id=$1`, job).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	hook := retention.FailureHook(retention.Deps{Pool: g.pool})
	n := queue.FailureNotice{JobID: job, JobType: "retention_enforce", WorkspaceID: g.ws, Payload: payload, Reason: "Heartbeat Timeout", WillRetry: true}
	if err := hook(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if s, e := g.runStatus(t, run); s != "pending" || !strings.HasPrefix(e, "Queue retry scheduled: Heartbeat Timeout") {
		t.Fatalf("run %s %q", s, e)
	}
}

// With the per-type flag off, a go-executor governance row that reaches the worker is
// handed back to Python (pending, executor python, no retry spent), never failed as an
// unsupported type; like the outreach and automations hand-back.
func TestGovernanceJobsHandBackWhenFlagsOff(t *testing.T) {
	g := setupGov(t)
	ctx := context.Background()
	cfg := govConfig(g.ws)
	cfg.Flags.Governance = config.GovernanceSettings{}
	rn := dispatch(governanceHandlers(cfg, g.pool, telemetry.NewLogger(io.Discard), nil, governanceOpts{}))
	for _, typ := range []string{"retention_enforce", "research_playbook_run", "research_playbook_schedule"} {
		if _, err := g.owner.Exec(ctx, `DELETE FROM jobs`); err != nil {
			t.Fatal(err)
		}
		var job int64
		if err := g.owner.QueryRow(ctx, `INSERT INTO jobs (type,payload,status,priority,created_at,retry_count,max_retries,workspace_id,executor)
			VALUES ($1,$2,'pending',0,now() AT TIME ZONE 'UTC',0,3,$3,'go') RETURNING id`, typ, `{"workspace_id":"`+g.ws+`","run_id":"r","playbook_id":"p"}`, g.ws).Scan(&job); err != nil {
			t.Fatal(err)
		}
		c, err := claimNext(ctx, g.pool, cfg, "w")
		if err != nil || c == nil || c.Type != typ {
			t.Fatalf("%s: claim %v err %v", typ, c, err)
		}
		if err := rn(ctx, c); !errors.Is(err, workbook.ErrHandedBack) {
			t.Fatalf("%s: err %v, want ErrHandedBack", typ, err)
		}
		var status, executor string
		var retries int
		if err := g.owner.QueryRow(ctx, `SELECT status, executor, retry_count FROM jobs WHERE id=$1`, job).Scan(&status, &executor, &retries); err != nil {
			t.Fatal(err)
		}
		if status != "pending" || executor != "python" || retries != 0 {
			t.Fatalf("%s: %s/%s retries %d", typ, status, executor, retries)
		}
	}
}
