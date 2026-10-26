//go:build integration

package main

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadjobs"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type probeFake struct{ calls int }

func (p *probeFake) SourceList(context.Context, time.Duration) ([]string, error) { return nil, nil }
func (p *probeFake) SourceProbe(context.Context, []string, time.Duration) (map[string]*int, error) {
	p.calls++
	return nil, nil
}

func runLeadgen(t *testing.T, cfg config.Config, py leadjobs.Prober, env map[string]string) (*pgxpool.Pool, *pgxpool.Pool) {
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
	t.Cleanup(func() { _, _ = owner.Exec(context.Background(), `DELETE FROM jobs WHERE type='source_health_check'`) })
	return pool, owner
}

func (e leadgenRun) drain(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	log := telemetry.NewLogger(io.Discard)
	rn := dispatch(leadgenHandlers(e.cfg, e.pool, log, nil, leadgenOpts{prober: e.py, getenv: func(k string) string { return e.env[k] }}))
	cctx, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	done := make(chan struct{})
	go func() { slotLoop(cctx, ctx, e.pool, e.cfg, log, "w", rn); close(done) }()
	for cctx.Err() == nil {
		var n int
		_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE executor='go' AND status IN ('pending','processing') AND (next_run_at IS NULL OR next_run_at <= now() AT TIME ZONE 'UTC')`).Scan(&n)
		if n == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	stop()
	<-done
}

type leadgenRun struct {
	cfg         config.Config
	pool, owner *pgxpool.Pool
	py          leadjobs.Prober
	env         map[string]string
}

func newLeadgenRun(t *testing.T, flag bool, py leadjobs.Prober) leadgenRun {
	pool, owner := runLeadgen(t, config.Config{}, py, nil)
	cfg := config.Config{HeartbeatInterval: time.Hour, JobTimeout: time.Minute, AppEnv: "test",
		Executor: config.ExecutorSettings{Enabled: flag, SourceHealth: flag}}
	return leadgenRun{cfg: cfg, pool: pool, owner: owner, py: py, env: map[string]string{"SOURCE_HEALTH_ENABLED": "1"}}
}

func (e leadgenRun) job(t *testing.T, executor string) int64 {
	var id int64
	if err := e.owner.QueryRow(context.Background(), `INSERT INTO jobs (executor,type,payload,priority,status,created_at,max_retries,retry_count)
		VALUES ($1,'source_health_check','{}',1,'pending',now() AT TIME ZONE 'UTC',3,0) RETURNING id`, executor).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e leadgenRun) row(t *testing.T, id int64) (status, executor string, retries int) {
	if err := e.owner.QueryRow(context.Background(), `SELECT status, executor, retry_count FROM jobs WHERE id=$1`, id).Scan(&status, &executor, &retries); err != nil {
		t.Fatal(err)
	}
	return
}

// Flag off: a go-executor row is handed back to Python with no retry spent, never failed or run.
func TestGoAndPythonNeverBothClaim(t *testing.T) {
	py := &probeFake{}
	e := newLeadgenRun(t, false, py)
	goJob, pyJob := e.job(t, "go"), e.job(t, "python")
	e.drain(t)
	if st, ex, rt := e.row(t, goJob); st != "pending" || ex != "python" || rt != 0 {
		t.Fatalf("go row %s/%s retries %d, want handed back", st, ex, rt)
	}
	if st, ex, _ := e.row(t, pyJob); st != "pending" || ex != "python" {
		t.Fatalf("python row touched: %s/%s", st, ex)
	}
	if py.calls != 0 {
		t.Fatal("probe ran with the executor flag off")
	}
}

// Flag on: the go row runs on Go (probe called, completed, next occurrence scheduled for go).
func TestWorkerRunsSourceHealthCheck(t *testing.T) {
	py := &probeFake{}
	e := newLeadgenRun(t, true, py)
	goJob := e.job(t, "go")
	e.drain(t)
	if st, ex, _ := e.row(t, goJob); st != "completed" || ex != "go" {
		t.Fatalf("go row %s/%s", st, ex)
	}
	var n int
	_ = e.owner.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE type='source_health_check' AND status='pending' AND executor='go' AND fire_key LIKE 'source_health:%'`).Scan(&n)
	if n != 1 {
		t.Fatalf("want one scheduled go successor, got %d", n)
	}
}

// No Python worker configured: the row is handed back rather than probed with nothing.
func TestSourceHealthWithoutPythonWorkerHandsBack(t *testing.T) {
	e := newLeadgenRun(t, true, nil)
	id := e.job(t, "go")
	e.drain(t)
	if st, ex, rt := e.row(t, id); st != "pending" || ex != "python" || rt != 0 {
		t.Fatalf("%s/%s %d", st, ex, rt)
	}
}
