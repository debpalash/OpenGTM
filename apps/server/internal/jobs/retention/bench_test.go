package retention

// retention_enforce benchmark (see benchmarks/README.md): the Go counterpart of
// benchmarks/bench_retention_python.py. It is a test only so it can reach the
// unexported worker without widening the production API, and it is skipped
// unless OPENGTM_BENCH_OUT is set; `benchmarks/run.sh --only retention` is the
// supported way to run it, against a throwaway database.
//
// For each row count it seeds a workspace with N expired and a few recent rows
// in each of the eight purged tables (benchmarks/retention_seed.sql) and
// measures two things, repeated OPENGTM_BENCH_REPS times:
//
//	go-handler  Worker.Handle for a claimed job: the handler alone, what
//	            Python's handle_retention_enforce does in-process.
//	go-job      a pending job claimed, run and finalized by the real queue
//	            (Queue.Run with a 5 ms idle poll), timed from insert to
//	            completed: the whole job lifecycle.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

type retentionBenchCase struct {
	Mode    string  `json:"mode"`
	Rows    int     `json:"rows"`
	Kept    int     `json:"kept"`
	Rep     int     `json:"rep"`
	WallMs  float64 `json:"wall_ms"`
	Deleted int64   `json:"deleted"`
}

func TestBenchRetention(t *testing.T) {
	out := os.Getenv("OPENGTM_BENCH_OUT")
	if out == "" {
		t.Skip("OPENGTM_BENCH_OUT not set; run benchmarks/run.sh --only retention")
	}
	seedSQL, err := os.ReadFile(os.Getenv("OPENGTM_BENCH_SEED_SQL"))
	if err != nil {
		t.Fatalf("OPENGTM_BENCH_SEED_SQL: %v", err)
	}
	owner := dbtest.OwnerURL(t)
	dbtest.Migrate(t, owner)
	role, password := os.Getenv("OPENGTM_BENCH_ROLE"), os.Getenv("OPENGTM_BENCH_ROLE_PASSWORD")
	if role == "" || password == "" {
		t.Fatal("OPENGTM_BENCH_ROLE and OPENGTM_BENCH_ROLE_PASSWORD are required")
	}
	appURL := dbtest.RoleURL(t, owner, role, password, dbtest.AppGroupRole)
	ownerPool, app := dbtest.Pool(t, owner, 2), dbtest.Pool(t, appURL, 8)

	var sizes []int
	for _, f := range strings.Split(os.Getenv("OPENGTM_BENCH_ROWS"), ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 1 {
			t.Fatalf("OPENGTM_BENCH_ROWS=%q", os.Getenv("OPENGTM_BENCH_ROWS"))
		}
		sizes = append(sizes, n)
	}
	reps, rep0, kept := benchInt("OPENGTM_BENCH_REPS", 1), benchInt("OPENGTM_BENCH_REP_OFFSET", 0), benchInt("OPENGTM_BENCH_KEPT", 100)

	ctx := context.Background()
	reset := func() {
		// Private throwaway database: start every case from empty tables.
		if _, err := ownerPool.Exec(ctx, `TRUNCATE governance_audit_events, llm_usage_daily, signals, destination_deliveries,
			destination_inbound_receipts, audience_membership_events, playbook_results, outreach_sends, playbook_runs,
			research_playbooks, destination_runs, audience_destinations, audiences, retention_runs, retention_policies,
			retention_schedules, jobs CASCADE`); err != nil {
			t.Fatal(err)
		}
	}
	seed := func(ws string, rows int) {
		conn, err := ownerPool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		// Simple protocol, so the multi-statement script is allowed.
		sql := string(seedSQL) + fmt.Sprintf("\nSELECT pg_temp.bench_seed(%s, %d, %d);", quote(ws), rows, kept)
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := conn.Exec(ctx, "ANALYZE"); err != nil {
			t.Fatal(err)
		}
	}
	verify := func(ws string, rows int) int64 {
		t.Helper()
		var status, counts string
		if err := ownerPool.QueryRow(ctx, `SELECT status, deleted_counts::text FROM retention_runs WHERE id = $1`, "run-"+ws).Scan(&status, &counts); err != nil {
			t.Fatal(err)
		}
		if status != "completed" {
			t.Fatalf("run status = %s", status)
		}
		var left, deleted int64
		for _, table := range []string{"governance_audit_events", "llm_usage_daily", "signals", "destination_deliveries",
			"destination_inbound_receipts", "audience_membership_events", "playbook_results", "outreach_sends"} {
			var n int64
			if err := ownerPool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE workspace_id = $1", ws).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != int64(kept) {
				t.Fatalf("%s left %d rows, want %d", table, n, kept)
			}
			left += n
		}
		var m map[string]int64
		if err := json.Unmarshal([]byte(counts), &m); err != nil {
			t.Fatal(err)
		}
		for _, v := range m {
			deleted += v
		}
		if deleted != int64(8*rows) {
			t.Fatalf("deleted %d rows, want %d", deleted, 8*rows)
		}
		return deleted
	}
	insertJob := func(ws, status string) queue.Job {
		raw, _ := json.Marshal(map[string]string{"workspace_id": ws, "run_id": "run-" + ws})
		job := queue.Job{Type: JobType, Payload: raw, WorkspaceID: ws, Lease: queue.Lease{WorkerID: "bench"}}
		if status == "processing" {
			err = ownerPool.QueryRow(ctx, `INSERT INTO jobs (type, payload, workspace_id, priority, status, created_at, next_run_at,
					max_retries, retry_count, worker_id, locked_at, last_heartbeat)
				VALUES ($1, $2::json, $3, 1, 'processing', LOCALTIMESTAMP, LOCALTIMESTAMP, 3, 0, 'bench', LOCALTIMESTAMP, LOCALTIMESTAMP)
				RETURNING id, locked_at`, JobType, string(raw), ws).Scan(&job.ID, &job.Lease.LockedAt)
		} else {
			err = ownerPool.QueryRow(ctx, `INSERT INTO jobs (type, payload, workspace_id, priority, status, created_at, next_run_at, max_retries, retry_count)
				VALUES ($1, $2::json, $3, 1, 'pending', LOCALTIMESTAMP, LOCALTIMESTAMP, 3, 0) RETURNING id`, JobType, string(raw), ws).Scan(&job.ID)
		}
		if err != nil {
			t.Fatal(err)
		}
		return job
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := NewWorker(app, quiet)

	if _, err := ownerPool.Exec(ctx, `INSERT INTO job_executor_routes (job_type, executor) VALUES ($1, 'go')
		ON CONFLICT (job_type) DO UPDATE SET executor = 'go'`, JobType); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = ownerPool.Exec(ctx, `DELETE FROM job_executor_routes WHERE job_type = $1`, JobType) }()
	reg := queue.NewRegistry()
	w.Register(reg)
	q := queue.New(app, reg, queue.Options{Concurrency: 1, IdlePoll: 5 * time.Millisecond, ClaimCheckInterval: time.Second,
		ReapInterval: time.Hour, Logger: quiet})
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = q.Run(runCtx) }()
	defer func() { stop(); <-done }()

	var cases []retentionBenchCase
	runOne := func(mode string, rows, rep int, record bool) {
		reset()
		ws := fmt.Sprintf("bench-%s-%d-%d", mode, rows, rep)
		seed(ws, rows)
		var wall time.Duration
		switch mode {
		case "go-handler":
			job := insertJob(ws, "processing")
			start := time.Now()
			if err := w.Handle(ctx, job); err != nil {
				t.Fatal(err)
			}
			wall = time.Since(start)
		case "go-job":
			start := time.Now()
			job := insertJob(ws, "pending")
			deadline := time.Now().Add(30 * time.Minute)
			for {
				var status string
				if err := ownerPool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, job.ID).Scan(&status); err != nil {
					t.Fatal(err)
				}
				if status == "completed" {
					break
				}
				if status == "failed" || time.Now().After(deadline) {
					t.Fatalf("job ended as %s", status)
				}
				time.Sleep(2 * time.Millisecond)
			}
			wall = time.Since(start)
		}
		deleted := verify(ws, rows)
		if record {
			cases = append(cases, retentionBenchCase{Mode: mode, Rows: rows, Kept: kept, Rep: rep, WallMs: float64(wall) / float64(time.Millisecond), Deleted: deleted})
			t.Logf("%s rows=%d rep=%d: %.0f ms (%d rows deleted)", mode, rows, rep, float64(wall)/float64(time.Millisecond), deleted)
		}
	}
	for _, mode := range []string{"go-handler", "go-job"} {
		runOne(mode, 500, 0, false) // warm-up, discarded
	}
	for rep := range reps {
		for _, rows := range sizes {
			for _, mode := range []string{"go-handler", "go-job"} {
				runOne(mode, rows, rep0+rep, true)
			}
		}
	}
	raw, err := json.MarshalIndent(map[string]any{"engine": "go", "go_version": runtime.Version(),
		"gomaxprocs": runtime.GOMAXPROCS(0), "cases": cases}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func benchInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			panic(fmt.Sprintf("%s=%q: %v", name, v, err))
		}
		return n
	}
	return def
}
