package queue

// Queue benchmark for the M0 baseline (see benchmarks/README.md).
//
// This is a test only so it can reach the unexported process() (handler
// invocation, lease monitor and guarded finalization) without widening the
// production API, and so the benchmark-only job type is never registered in
// production. It is skipped unless OPENGTM_BENCH_OUT is set; benchmarks/run.sh
// is the supported way to run it, against a throwaway database.
//
// Each concurrency slot mirrors slotLoop: Claim, then process. The only
// deliberate difference is that an empty claim retries after 1 ms instead of
// IdlePoll (1 s), because the backlog is finite and the idle poll would
// otherwise dominate the tail of a short run. The Python harness does the same.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
)

type benchStats struct {
	N    int     `json:"n"`
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
}

type benchCase struct {
	Engine        string     `json:"engine"`
	Mode          string     `json:"mode"`
	Concurrency   int        `json:"concurrency"`
	Rep           int        `json:"rep"`
	Jobs          int        `json:"jobs"`
	WallSeconds   float64    `json:"wall_s"`
	JobsPerSec    float64    `json:"jobs_per_sec"`
	EnqueuePerSec float64    `json:"enqueue_per_sec"`
	ClaimMs       benchStats `json:"claim_ms"`
	ProcessMs     benchStats `json:"process_ms"`
	CycleMs       benchStats `json:"cycle_ms"`
	Failed        int        `json:"failed"`
}

func benchEnvInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			panic(fmt.Sprintf("%s=%q: %v", name, v, err))
		}
		return n
	}
	return def
}

func summarize(samples []time.Duration) benchStats {
	if len(samples) == 0 {
		return benchStats{}
	}
	ms := make([]float64, len(samples))
	var sum float64
	for i, d := range samples {
		ms[i] = float64(d) / float64(time.Millisecond)
		sum += ms[i]
	}
	slices.Sort(ms)
	// Nearest-rank percentile, the same definition the Python harness uses.
	pick := func(p float64) float64 {
		rank := int(p/100*float64(len(ms))+0.999999999) - 1
		return ms[min(max(rank, 0), len(ms)-1)]
	}
	return benchStats{N: len(ms), Mean: sum / float64(len(ms)),
		P50: pick(50), P95: pick(95), P99: pick(99), Max: ms[len(ms)-1]}
}

func TestBenchQueue(t *testing.T) {
	out := os.Getenv("OPENGTM_BENCH_OUT")
	if out == "" {
		t.Skip("OPENGTM_BENCH_OUT not set; run benchmarks/run.sh")
	}
	owner := dbtest.OwnerURL(t)
	dbtest.Migrate(t, owner)
	role := os.Getenv("OPENGTM_BENCH_ROLE")
	if role == "" {
		role = "opengtm_bench_app"
	}
	password := os.Getenv("OPENGTM_BENCH_ROLE_PASSWORD")
	if password == "" {
		password = "bench_only_pw"
	}
	appURL := dbtest.RoleURL(t, owner, role, password, dbtest.AppGroupRole)

	jobs := benchEnvInt("OPENGTM_BENCH_JOBS", 2000)
	warmup := benchEnvInt("OPENGTM_BENCH_WARMUP", 200)
	reps := benchEnvInt("OPENGTM_BENCH_REPS", 1)
	rep0 := benchEnvInt("OPENGTM_BENCH_REP_OFFSET", 0)
	levels := []int{1, 4, 16}
	if v := os.Getenv("OPENGTM_BENCH_CONCURRENCY"); v != "" {
		levels = nil
		for _, f := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil || n < 1 {
				t.Fatalf("OPENGTM_BENCH_CONCURRENCY=%q", v)
			}
			levels = append(levels, n)
		}
	}

	ctx := context.Background()
	ownerConn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerConn.Close(ctx)
	reset := func() {
		// Private throwaway database: start every case from an empty table.
		if _, err := ownerConn.Exec(ctx, "TRUNCATE jobs"); err != nil {
			t.Fatal(err)
		}
		if _, err := ownerConn.Exec(ctx, "VACUUM ANALYZE jobs"); err != nil {
			t.Fatal(err)
		}
	}

	var cases []benchCase
	for rep := range reps {
		for _, c := range levels {
			pool := dbtest.Pool(t, appURL, int32(2*c+4))
			jobType := fmt.Sprintf("bench_noop_go_c%d_r%d", c, rep0+rep)
			if _, err := pool.Exec(ctx, `INSERT INTO job_executor_routes (job_type, executor)
				VALUES ($1, 'go') ON CONFLICT (job_type) DO UPDATE SET executor = 'go'`, jobType); err != nil {
				t.Fatal(err)
			}
			q := New(pool, nil, Options{Concurrency: c, Logger: quietLogger()})
			q.Register(jobType, func(context.Context, Job) error { return nil })

			reset()
			runBenchCase(t, pool, q, jobType, c, warmup) // warm-up, discarded
			reset()
			res := runBenchCase(t, pool, q, jobType, c, jobs)
			res.Rep = rep0 + rep
			cases = append(cases, res)
			t.Logf("go c=%d rep=%d: %.0f jobs/s, cycle p50=%.2fms p99=%.2fms", c, res.Rep,
				res.JobsPerSec, res.CycleMs.P50, res.CycleMs.P99)
			pool.Close()
		}
	}

	raw, err := json.MarshalIndent(map[string]any{
		"engine": "go", "mode": "go-queue", "go_version": runtime.Version(),
		"gomaxprocs": runtime.GOMAXPROCS(0), "cases": cases,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runBenchCase(t *testing.T, pool *pgxpool.Pool, q *Queue, jobType string, conc, n int) benchCase {
	t.Helper()
	ctx := context.Background()

	start := time.Now()
	for range n {
		if _, err := Enqueue(ctx, pool, jobType, map[string]any{}, EnqueueOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	enqueueRate := float64(n) / time.Since(start).Seconds()
	if _, err := pool.Exec(ctx, "ANALYZE jobs"); err != nil {
		t.Fatal(err)
	}

	var (
		done            atomic.Int64
		failed          atomic.Int64
		mu              sync.Mutex
		claims, procs   = make([]time.Duration, 0, n), make([]time.Duration, 0, n)
		cycles          = make([]time.Duration, 0, n)
		wg              sync.WaitGroup
		attempts        = context.Background()
		first, lastDone time.Time
	)
	for range conc {
		wg.Go(func() {
			for done.Load() < int64(n) {
				t0 := time.Now()
				job, err := q.Claim(ctx)
				t1 := time.Now()
				if err != nil {
					failed.Add(1)
					time.Sleep(time.Millisecond)
					continue
				}
				if job == nil {
					time.Sleep(time.Millisecond)
					continue
				}
				q.process(attempts, *job)
				t2 := time.Now()
				mu.Lock()
				claims = append(claims, t1.Sub(t0))
				procs = append(procs, t2.Sub(t1))
				cycles = append(cycles, t2.Sub(t0))
				if first.IsZero() {
					first = t0
				}
				lastDone = t2
				mu.Unlock()
				done.Add(1)
			}
		})
	}
	wg.Wait()
	wall := lastDone.Sub(first)

	var completed int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM jobs WHERE type = $1 AND status = 'completed'",
		jobType).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if completed != n {
		t.Fatalf("completed %d of %d jobs", completed, n)
	}
	return benchCase{
		Engine: "go", Mode: "go-queue", Concurrency: conc, Jobs: n,
		WallSeconds: wall.Seconds(), JobsPerSec: float64(n) / wall.Seconds(),
		EnqueuePerSec: enqueueRate,
		ClaimMs:       summarize(claims), ProcessMs: summarize(procs), CycleMs: summarize(cycles),
		Failed: int(failed.Load()),
	}
}
