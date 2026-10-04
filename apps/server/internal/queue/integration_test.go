package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
)

// These tests run against the real Alembic schema as a NOSUPERUSER
// NOBYPASSRLS member of the runtime group. Every test uses its own job types
// (and workspaces) so packages and tests sharing the database never collide.

var (
	sharedOnce sync.Once
	sharedPool *pgxpool.Pool
	ownerURL   string
)

func appPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	owner := dbtest.OwnerURL(t)
	sharedOnce.Do(func() {
		dbtest.Migrate(t, owner)
		ownerURL = owner
		cfg, err := pgxpool.ParseConfig(dbtest.AppURL(t, owner))
		if err != nil {
			t.Fatal(err)
		}
		cfg.MaxConns = 60
		sharedPool, err = pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
	})
	if sharedPool == nil {
		t.Fatal("database setup failed earlier")
	}
	return sharedPool
}

func tag(t *testing.T) string {
	var b [5]byte
	rand.Read(b[:])
	return "t" + hex.EncodeToString(b[:])
}

func quietLogger() *slog.Logger {
	if os.Getenv("OPENGTM_TEST_VERBOSE") != "" {
		return slog.Default()
	}
	return slog.New(slog.DiscardHandler)
}

// routeGo routes types to Go for the duration of the test and removes their
// jobs afterwards.
func routeGo(t *testing.T, pool *pgxpool.Pool, types ...string) {
	t.Helper()
	ctx := context.Background()
	for _, jt := range types {
		if _, err := pool.Exec(ctx, `INSERT INTO job_executor_routes (job_type, executor) VALUES ($1, 'go')
			ON CONFLICT (job_type) DO UPDATE SET executor = 'go'`, jt); err != nil {
			t.Fatal(err)
		}
	}
	cleanupTypes(t, pool, types...)
}

func cleanupTypes(t *testing.T, pool *pgxpool.Pool, types ...string) {
	t.Cleanup(func() {
		ctx := context.Background()
		pool.Exec(ctx, `DELETE FROM jobs WHERE type = ANY($1)`, types)
		pool.Exec(ctx, `DELETE FROM job_executor_routes WHERE job_type = ANY($1)`, types)
	})
}

func newQueue(pool *pgxpool.Pool, opts Options, types ...string) *Queue {
	if opts.Logger == nil {
		opts.Logger = quietLogger()
	}
	q := New(pool, nil, opts)
	for _, jt := range types {
		q.Register(jt, func(context.Context, Job) error { return nil })
	}
	return q
}

func enqueue(t *testing.T, pool *pgxpool.Pool, jobType string, payload any, opts EnqueueOptions) int64 {
	t.Helper()
	id, err := Enqueue(context.Background(), pool, jobType, payload, opts)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

type jobRow struct {
	Status                 string
	RetryCount, MaxRetries *int32
	Error, WorkerID        *string
	WorkspaceID            *string
	Priority               *int32
	LockedAt, CompletedAt  *time.Time
	NextRunAt, Heartbeat   *time.Time
	StartedAt              *time.Time
	Now                    time.Time
}

func getJob(t *testing.T, pool *pgxpool.Pool, id int64) jobRow {
	t.Helper()
	var r jobRow
	err := pool.QueryRow(context.Background(), `
SELECT status, retry_count, max_retries, error, worker_id, workspace_id, priority, locked_at,
       completed_at, next_run_at, last_heartbeat, started_at, LOCALTIMESTAMP
FROM jobs WHERE id = $1`, id).Scan(&r.Status, &r.RetryCount, &r.MaxRetries, &r.Error, &r.WorkerID,
		&r.WorkspaceID, &r.Priority, &r.LockedAt, &r.CompletedAt, &r.NextRunAt, &r.Heartbeat, &r.StartedAt, &r.Now)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func claim(t *testing.T, q *Queue) *Job {
	t.Helper()
	job, err := q.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestEnqueueMirrorsAddJob(t *testing.T) {
	pool := appPool(t)
	jt := tag(t)
	cleanupTypes(t, pool, jt)
	id := enqueue(t, pool, jt, map[string]any{"workspace_id": "ws-x", "n": 1}, EnqueueOptions{})
	r := getJob(t, pool, id)
	if r.Status != "pending" || str(r.WorkspaceID) != "ws-x" || *r.Priority != 1 || *r.MaxRetries != 3 || *r.RetryCount != 0 {
		t.Fatalf("unexpected row: %+v", r)
	}
	if r.NextRunAt == nil || r.Now.Sub(*r.NextRunAt) < 0 || r.Now.Sub(*r.NextRunAt) > 5*time.Second {
		t.Fatalf("next_run_at should be now: %v vs %v", r.NextRunAt, r.Now)
	}

	two := 0
	id = enqueue(t, pool, jt, json.RawMessage(`{"a":1}`), EnqueueOptions{Priority: 7, MaxRetries: &two, RunAt: time.Now().Add(time.Hour)})
	r = getJob(t, pool, id)
	if r.WorkspaceID != nil || *r.Priority != 7 || *r.MaxRetries != 0 {
		t.Fatalf("unexpected row: %+v", r)
	}
	if d := r.NextRunAt.Sub(r.Now); d < 59*time.Minute || d > 61*time.Minute {
		t.Fatalf("RunAt not honoured: next_run_at - now = %v", d)
	}

	ctx := context.Background()
	key := jt + ":fire"
	enqueue(t, pool, jt, nil, EnqueueOptions{FireKey: key})
	if _, err := Enqueue(ctx, pool, jt, nil, EnqueueOptions{FireKey: key}); !errors.Is(err, ErrDuplicateFireKey) {
		t.Fatalf("duplicate fire_key err = %v", err)
	}
	if _, err := Enqueue(ctx, pool, jt, []int{1}, EnqueueOptions{}); err == nil {
		t.Fatal("non-object payload accepted")
	}

	// Enqueue participates in the caller's transaction.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rolled, err := Enqueue(ctx, tx, jt, nil, EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tx.Rollback(ctx)
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE id = $1`, rolled).Scan(&n)
	if n != 0 {
		t.Fatal("job from a rolled-back transaction is visible")
	}
}

func TestConcurrentClaimersNeverDoubleClaim(t *testing.T) {
	pool := appPool(t)
	jt := tag(t)
	routeGo(t, pool, jt)
	const jobs, workers = 200, 8
	want := map[int64]bool{}
	for i := range jobs {
		want[enqueue(t, pool, jt, map[string]any{"i": i}, EnqueueOptions{})] = true
	}

	var mu sync.Mutex
	owners := map[int64][]string{}
	var errs []error
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range workers {
		q := newQueue(pool, Options{MaxActivePerWorkspace: 0}, jt)
		wg.Go(func() {
			<-start
			for {
				job, err := q.Claim(context.Background())
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					return
				}
				if job == nil {
					return
				}
				mu.Lock()
				owners[job.ID] = append(owners[job.ID], job.Lease.WorkerID)
				mu.Unlock()
			}
		})
	}
	close(start)
	wg.Wait()
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if len(owners) != jobs {
		t.Fatalf("claimed %d distinct jobs, want %d", len(owners), jobs)
	}
	for id, ws := range owners {
		if len(ws) != 1 || !want[id] {
			t.Fatalf("job %d claimed by %v", id, ws)
		}
		r := getJob(t, pool, id)
		if r.Status != "processing" || str(r.WorkerID) != ws[0] || r.LockedAt == nil || r.StartedAt == nil || r.Heartbeat == nil {
			t.Fatalf("job %d row does not reflect the claim: %+v", id, r)
		}
	}
}

func TestTenantCapHoldsUnderConcurrency(t *testing.T) {
	pool := appPool(t)
	jt := tag(t)
	routeGo(t, pool, jt)
	wsA, wsB := jt+"-a", jt+"-b"
	for i := range 30 {
		enqueue(t, pool, jt, map[string]any{"workspace_id": wsA, "i": i}, EnqueueOptions{})
		enqueue(t, pool, jt, map[string]any{"workspace_id": wsB, "i": i}, EnqueueOptions{})
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 16)
	for range 8 {
		q := newQueue(pool, Options{MaxActivePerWorkspace: 2}, jt)
		wg.Go(func() {
			<-start
			for {
				job, err := q.Claim(context.Background())
				if err != nil {
					errs <- err
					return
				}
				if job == nil {
					return
				}
			}
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	rows, _ := pool.Query(context.Background(), `SELECT workspace_id, count(*) FROM jobs
		WHERE type = $1 AND status = 'processing' GROUP BY workspace_id`, jt)
	got := map[string]int{}
	var ws string
	var n int
	pgx.ForEachRow(rows, []any{&ws, &n}, func() error { got[ws] = n; return nil })
	if got[wsA] != 2 || got[wsB] != 2 || len(got) != 2 {
		t.Fatalf("processing per workspace = %v, want exactly 2 each", got)
	}
}

func TestFairnessPrefersLeastBusyWorkspace(t *testing.T) {
	pool := appPool(t)
	jt := tag(t)
	routeGo(t, pool, jt)
	q := newQueue(pool, Options{MaxActivePerWorkspace: 5}, jt)
	busy, idle := jt+"-busy", jt+"-idle"
	enqueue(t, pool, jt, map[string]any{"workspace_id": busy}, EnqueueOptions{})
	first := claim(t, q)
	if first == nil || first.WorkspaceID != busy {
		t.Fatalf("claimed %+v", first)
	}
	older := enqueue(t, pool, jt, map[string]any{"workspace_id": busy}, EnqueueOptions{})
	newer := enqueue(t, pool, jt, map[string]any{"workspace_id": idle}, EnqueueOptions{})
	if got := claim(t, q); got.ID != newer {
		t.Fatalf("claimed %d, want idle workspace job %d before busy %d", got.ID, newer, older)
	}
	high := enqueue(t, pool, jt, map[string]any{"workspace_id": busy}, EnqueueOptions{Priority: 9})
	if got := claim(t, q); got.ID != high {
		t.Fatalf("priority must dominate fairness: claimed %d, want %d", got.ID, high)
	}
}

func TestRetryBackoffAndFinalFailure(t *testing.T) {
	pool := appPool(t)
	jt := tag(t)
	routeGo(t, pool, jt)
	q := newQueue(pool, Options{}, jt)
	type call struct {
		reason    string
		willRetry bool
	}
	var calls []call
	q.RegisterFailure(jt, func(_ context.Context, _ Job, reason string, willRetry bool) error {
		calls = append(calls, call{reason, willRetry})
		return nil
	})
	two := 2
	id := enqueue(t, pool, jt, nil, EnqueueOptions{MaxRetries: &two})
	log := quietLogger()

	for attempt, wantBackoff := range []time.Duration{time.Minute, 2 * time.Minute} {
		job := claim(t, q)
		if job == nil || job.ID != id {
			t.Fatalf("attempt %d: claimed %+v", attempt, job)
		}
		q.finish(*job, errors.New("boom"), log)
		r := getJob(t, pool, id)
		wantErr := fmt.Sprintf("Retry %d: boom", attempt+1)
		if r.Status != "pending" || *r.RetryCount != int32(attempt+1) || str(r.Error) != wantErr ||
			r.WorkerID != nil || r.LockedAt != nil || r.CompletedAt != nil {
			t.Fatalf("attempt %d: unexpected row %+v (error %q)", attempt, r, str(r.Error))
		}
		if d := r.NextRunAt.Sub(r.Now); d < wantBackoff-5*time.Second || d > wantBackoff {
			t.Fatalf("attempt %d: backoff %v, want ~%v", attempt, d, wantBackoff)
		}
		if claim(t, q) != nil {
			t.Fatal("job claimable before its backoff elapsed")
		}
		mustExec(t, pool, `UPDATE jobs SET next_run_at = LOCALTIMESTAMP WHERE id = $1`, id)
	}
	job := claim(t, q)
	q.finish(*job, errors.New("boom"), log)
	r := getJob(t, pool, id)
	if r.Status != "failed" || str(r.Error) != "Final Failure: boom" || r.CompletedAt == nil || *r.RetryCount != 2 {
		t.Fatalf("final failure row: %+v (error %q)", r, str(r.Error))
	}
	want := []call{{"boom", true}, {"boom", true}, {"boom", false}}
	if !slices.Equal(calls, want) {
		t.Fatalf("failure handler calls = %v, want %v", calls, want)
	}

	// NULL max_retries means 3; success clears the error.
	id = enqueue(t, pool, jt, nil, EnqueueOptions{})
	mustExec(t, pool, `UPDATE jobs SET max_retries = NULL, retry_count = 3, error = 'old' WHERE id = $1`, id)
	job = claim(t, q)
	q.finish(*job, errors.New("late"), log)
	if r := getJob(t, pool, id); r.Status != "failed" || str(r.Error) != "Final Failure: late" {
		t.Fatalf("NULL max_retries should allow 3 retries: %+v", r)
	}
	id = enqueue(t, pool, jt, nil, EnqueueOptions{})
	mustExec(t, pool, `UPDATE jobs SET error = 'old' WHERE id = $1`, id)
	job = claim(t, q)
	q.finish(*job, nil, log)
	if r := getJob(t, pool, id); r.Status != "completed" || r.Error != nil || r.CompletedAt == nil || r.WorkerID != nil {
		t.Fatalf("success row: %+v", r)
	}
}

func TestCancellationIsPreservedAndStopsHandler(t *testing.T) {
	pool := appPool(t)
	jt := tag(t)
	routeGo(t, pool, jt)
	q := newQueue(pool, Options{ClaimCheckInterval: 50 * time.Millisecond}, jt)
	failures := 0
	q.RegisterFailure(jt, func(context.Context, Job, string, bool) error { failures++; return nil })
	stopped := make(chan error, 1)
	q.Register(jt, func(ctx context.Context, job Job) error {
		<-ctx.Done()
		stopped <- context.Cause(ctx)
		return ctx.Err()
	})
	id := enqueue(t, pool, jt, nil, EnqueueOptions{})
	job := claim(t, q)

	done := make(chan struct{})
	go func() {
		q.process(context.Background(), *job)
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)
	// What the API cancel endpoint does.
	mustExec(t, pool, `UPDATE jobs SET status = 'cancelled' WHERE id = $1`, id)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler was not stopped promptly after cancellation")
	}
	if cause := <-stopped; !errors.Is(cause, errLeaseLost) {
		t.Fatalf("handler context cause = %v", cause)
	}
	r := getJob(t, pool, id)
	if r.Status != "cancelled" || r.CompletedAt == nil || r.WorkerID != nil || r.LockedAt != nil {
		t.Fatalf("cancellation not preserved: %+v", r)
	}
	if failures != 0 {
		t.Fatal("failure handler ran for an externally cancelled job")
	}

	// A late success must not overwrite a committed cancellation either.
	id = enqueue(t, pool, jt, nil, EnqueueOptions{})
	job = claim(t, q)
	mustExec(t, pool, `UPDATE jobs SET status = 'cancelled', completed_at = LOCALTIMESTAMP - interval '1 hour' WHERE id = $1`, id)
	before := getJob(t, pool, id).CompletedAt
	q.finish(*job, nil, quietLogger())
	if r := getJob(t, pool, id); r.Status != "cancelled" || !r.CompletedAt.Equal(*before) {
		t.Fatalf("success overwrote cancellation: %+v", r)
	}
}

func TestTimeoutFailsAttempt(t *testing.T) {
	pool := appPool(t)
	jt := tag(t)
	routeGo(t, pool, jt)
	q := newQueue(pool, Options{Timeouts: map[string]time.Duration{jt: 200 * time.Millisecond}}, jt)
	q.Register(jt, func(ctx context.Context, job Job) error {
		<-ctx.Done()
		return ctx.Err()
	})
	id := enqueue(t, pool, jt, nil, EnqueueOptions{})
	q.process(context.Background(), *claim(t, q))
	r := getJob(t, pool, id)
	want := fmt.Sprintf("Retry 1: job %d (%s) exceeded 0.2s", id, jt)
	if r.Status != "pending" || str(r.Error) != want {
		t.Fatalf("timeout row: status %s error %q, want %q", r.Status, str(r.Error), want)
	}

	// A handler that ignores cancellation is finalized after HandlerStopGrace.
	q.opts.HandlerStopGrace = 100 * time.Millisecond
	release := make(chan struct{})
	defer close(release)
	q.Register(jt, func(ctx context.Context, job Job) error { <-release; return nil })
	mustExec(t, pool, `UPDATE jobs SET next_run_at = LOCALTIMESTAMP WHERE id = $1`, id)
	start := time.Now()
	q.process(context.Background(), *claim(t, q))
	if time.Since(start) > 2*time.Second {
		t.Fatal("uncooperative handler blocked finalization")
	}
	if r := getJob(t, pool, id); r.Status != "pending" || *r.RetryCount != 2 {
		t.Fatalf("uncooperative timeout row: %+v", r)
	}

	// Returning nil after the deadline does not turn a timeout into success.
	q.opts.HandlerStopGrace = 2 * time.Second
	q.Register(jt, func(ctx context.Context, job Job) error { time.Sleep(400 * time.Millisecond); return nil })
	mustExec(t, pool, `UPDATE jobs SET next_run_at = LOCALTIMESTAMP WHERE id = $1`, id)
	q.process(context.Background(), *claim(t, q))
	want = fmt.Sprintf("Retry 3: job %d (%s) exceeded 0.2s", id, jt)
	if r := getJob(t, pool, id); r.Status != "pending" || str(r.Error) != want {
		t.Fatalf("late nil after timeout: status %s error %q, want %q", r.Status, str(r.Error), want)
	}
}

func TestHandlerPanicIsAFailure(t *testing.T) {
	pool := appPool(t)
	jt := tag(t)
	routeGo(t, pool, jt)
	q := newQueue(pool, Options{}, jt)
	q.Register(jt, func(context.Context, Job) error { panic("kaboom") })
	id := enqueue(t, pool, jt, nil, EnqueueOptions{})
	q.process(context.Background(), *claim(t, q))
	if r := getJob(t, pool, id); r.Status != "pending" || str(r.Error) != "Retry 1: handler panic: kaboom" {
		t.Fatalf("panic row: %+v (%q)", r, str(r.Error))
	}
}

func TestHeartbeatRenewsLease(t *testing.T) {
	pool := appPool(t)
	jt := tag(t)
	routeGo(t, pool, jt)
	q := newQueue(pool, Options{HeartbeatInterval: 100 * time.Millisecond}, jt)
	beats := make(chan time.Time, 1)
	q.Register(jt, func(ctx context.Context, job Job) error {
		time.Sleep(400 * time.Millisecond)
		var hb time.Time
		pool.QueryRow(ctx, `SELECT last_heartbeat FROM jobs WHERE id = $1`, job.ID).Scan(&hb)
		beats <- hb
		return nil
	})
	enqueue(t, pool, jt, nil, EnqueueOptions{})
	job := claim(t, q)
	q.process(context.Background(), *job)
	if hb := <-beats; !hb.After(job.Lease.LockedAt) {
		t.Fatalf("heartbeat %v not renewed past claim %v", hb, job.Lease.LockedAt)
	}
}

func TestLeaseFencingAfterReclaim(t *testing.T) {
	for _, sameWorker := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_worker=%v", sameWorker), func(t *testing.T) {
			pool := appPool(t)
			jt := tag(t)
			routeGo(t, pool, jt)
			stale := newQueue(pool, Options{}, jt)
			replacementOpts := Options{}
			if sameWorker {
				// A restarted process can never reuse an id, but the lease
				// must not rely on that: locked_at alone must fence it.
				replacementOpts.WorkerID = stale.WorkerID()
			}
			replacement := newQueue(pool, replacementOpts, jt)
			reaper := newQueue(pool, Options{}, jt)
			ctx := context.Background()

			id := enqueue(t, pool, jt, nil, EnqueueOptions{})
			old := claim(t, stale)
			mustExec(t, pool, `UPDATE jobs SET last_heartbeat = LOCALTIMESTAMP - interval '10 minutes' WHERE id = $1`, id)
			if n, err := reaper.ReapOnce(ctx); err != nil || n != 1 {
				t.Fatalf("ReapOnce = %d, %v", n, err)
			}
			mustExec(t, pool, `UPDATE jobs SET next_run_at = LOCALTIMESTAMP WHERE id = $1`, id)
			time.Sleep(2 * time.Millisecond) // distinct locked_at even at microsecond resolution
			fresh := claim(t, replacement)
			if fresh == nil || fresh.ID != id || fresh.Lease.LockedAt.Equal(old.Lease.LockedAt) {
				t.Fatalf("replacement claim %+v", fresh)
			}

			res, err := pool.Exec(ctx, renewSQL, id, old.Lease.WorkerID, old.Lease.LockedAt)
			if err != nil || res.RowsAffected() != 0 {
				t.Fatalf("stale heartbeat renewed the replacement claim (%v)", err)
			}
			var active bool
			err = pool.QueryRow(ctx, claimActiveSQL, id, old.Lease.WorkerID, old.Lease.LockedAt).Scan(&active)
			if !errors.Is(err, pgx.ErrNoRows) && active {
				t.Fatal("stale attempt still sees its claim as active")
			}
			for _, failure := range []error{nil, errors.New("stale")} {
				o, err := stale.finalize(ctx, *old, failure)
				if err != nil || o.persisted {
					t.Fatalf("stale finalize persisted=%v err=%v", o.persisted, err)
				}
			}
			r := getJob(t, pool, id)
			if r.Status != "processing" || str(r.WorkerID) != fresh.Lease.WorkerID || !r.LockedAt.Equal(fresh.Lease.LockedAt) {
				t.Fatalf("stale attempt modified the replacement claim: %+v", r)
			}
			if o, err := replacement.finalize(ctx, *fresh, nil); err != nil || !o.persisted {
				t.Fatalf("replacement finalize persisted=%v err=%v", o.persisted, err)
			}
			if r := getJob(t, pool, id); r.Status != "completed" {
				t.Fatalf("replacement result lost: %+v", r)
			}
		})
	}
}

func TestReaperCompareAndSwap(t *testing.T) {
	pool := appPool(t)
	jt, pyType := tag(t), tag(t)
	routeGo(t, pool, jt)
	cleanupTypes(t, pool, pyType)
	q := newQueue(pool, Options{}, jt, pyType)
	var calls []string
	q.RegisterFailure(jt, func(_ context.Context, job Job, reason string, retry bool) error {
		calls = append(calls, fmt.Sprintf("%d:%s:%v", job.ID, reason, retry))
		return nil
	})
	ctx := context.Background()
	stale := func(id int64, retries, limit int) {
		mustExec(t, pool, `UPDATE jobs SET status = 'processing', worker_id = 'dead:1:00000000',
			locked_at = LOCALTIMESTAMP - interval '1 hour', started_at = LOCALTIMESTAMP - interval '1 hour',
			last_heartbeat = LOCALTIMESTAMP - interval '6 minutes', retry_count = $2, max_retries = $3
			WHERE id = $1`, id, retries, limit)
	}
	retryable := enqueue(t, pool, jt, nil, EnqueueOptions{})
	stale(retryable, 1, 3)
	exhausted := enqueue(t, pool, jt, nil, EnqueueOptions{})
	stale(exhausted, 2, 2)
	neverBeat := enqueue(t, pool, jt, nil, EnqueueOptions{})
	stale(neverBeat, 0, 3)
	mustExec(t, pool, `UPDATE jobs SET last_heartbeat = NULL WHERE id = $1`, neverBeat)
	alive := enqueue(t, pool, jt, nil, EnqueueOptions{})
	stale(alive, 0, 3)
	mustExec(t, pool, `UPDATE jobs SET last_heartbeat = LOCALTIMESTAMP WHERE id = $1`, alive)
	pythonOwned := enqueue(t, pool, pyType, nil, EnqueueOptions{})
	stale(pythonOwned, 0, 3)

	n, err := q.ReapOnce(ctx)
	if err != nil || n != 3 {
		t.Fatalf("ReapOnce = %d, %v; want 3", n, err)
	}
	r := getJob(t, pool, retryable)
	if r.Status != "pending" || *r.RetryCount != 2 || str(r.Error) != "Heartbeat Timeout" ||
		r.WorkerID != nil || r.LockedAt != nil || r.StartedAt != nil || r.Heartbeat != nil || r.CompletedAt != nil {
		t.Fatalf("retryable row: %+v (%q)", r, str(r.Error))
	}
	// Python's reaper backs off 2^previous_retries minutes.
	if d := r.NextRunAt.Sub(r.Now); d < 2*time.Minute-5*time.Second || d > 2*time.Minute {
		t.Fatalf("reaper backoff %v, want ~2m", d)
	}
	r = getJob(t, pool, exhausted)
	if r.Status != "failed" || *r.RetryCount != 2 || str(r.Error) != "Final Failure: Heartbeat Timeout" || r.CompletedAt == nil || r.NextRunAt != nil {
		t.Fatalf("exhausted row: %+v (%q)", r, str(r.Error))
	}
	if r := getJob(t, pool, neverBeat); r.Status != "pending" {
		t.Fatalf("NULL heartbeat row not recovered: %+v", r)
	}
	if r := getJob(t, pool, alive); r.Status != "processing" {
		t.Fatalf("live claim reaped: %+v", r)
	}
	if r := getJob(t, pool, pythonOwned); r.Status != "processing" {
		t.Fatalf("Go reaped a Python-routed job: %+v", r)
	}
	slices.Sort(calls)
	want := []string{
		fmt.Sprintf("%d:Heartbeat Timeout:true", retryable),
		fmt.Sprintf("%d:Heartbeat Timeout:false", exhausted),
		fmt.Sprintf("%d:Heartbeat Timeout:true", neverBeat),
	}
	slices.Sort(want)
	if !slices.Equal(calls, want) {
		t.Fatalf("failure reconciliation = %v, want %v", calls, want)
	}

	// The CAS loses to a concurrent heartbeat: re-running the update with the
	// observed (now outdated) heartbeat must not touch the row.
	id := enqueue(t, pool, jt, nil, EnqueueOptions{})
	stale(id, 0, 3)
	before := getJob(t, pool, id)
	mustExec(t, pool, `UPDATE jobs SET last_heartbeat = LOCALTIMESTAMP WHERE id = $1`, id)
	res, err := pool.Exec(ctx, recoverCASSQL, id, before.WorkerID, before.LockedAt, before.Heartbeat,
		before.RetryCount, "pending", int32(1), nil, nil, "Heartbeat Timeout")
	if err != nil || res.RowsAffected() != 0 {
		t.Fatalf("CAS overwrote a renewed claim (%v, %d rows)", err, res.RowsAffected())
	}
}

func TestStartupRecoveryScope(t *testing.T) {
	pool := appPool(t)
	jt, pyType := tag(t), tag(t)
	routeGo(t, pool, jt)
	cleanupTypes(t, pool, pyType)
	q := newQueue(pool, Options{}, jt, pyType)
	processing := func(id int64, owner *string) {
		mustExec(t, pool, `UPDATE jobs SET status = 'processing', worker_id = $2, locked_at = LOCALTIMESTAMP,
			last_heartbeat = LOCALTIMESTAMP WHERE id = $1`, id, owner)
	}
	self, other := q.WorkerID(), "other:1:abcdef01"
	mine := enqueue(t, pool, jt, nil, EnqueueOptions{})
	processing(mine, &self)
	orphanGo := enqueue(t, pool, jt, nil, EnqueueOptions{})
	processing(orphanGo, nil)
	orphanPy := enqueue(t, pool, pyType, nil, EnqueueOptions{})
	processing(orphanPy, nil)
	others := enqueue(t, pool, jt, nil, EnqueueOptions{})
	processing(others, &other)

	if err := q.RecoverOwned(context.Background()); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]string{mine: "pending", orphanGo: "pending", orphanPy: "processing", others: "processing"} {
		r := getJob(t, pool, id)
		if r.Status != want {
			t.Errorf("job %d: status %s, want %s", id, r.Status, want)
		}
		if want == "pending" && str(r.Error) != "Recovered from crash" {
			t.Errorf("job %d: error %q", id, str(r.Error))
		}
	}
}

func TestGoNeverClaimsPythonRoutedJobs(t *testing.T) {
	pool := appPool(t)
	goType, pyExplicit, unrouted := tag(t), tag(t), tag(t)
	routeGo(t, pool, goType)
	mustExec(t, pool, `INSERT INTO job_executor_routes (job_type, executor) VALUES ($1, 'python')`, pyExplicit)
	cleanupTypes(t, pool, pyExplicit, unrouted)
	// Go has handlers for all three; routing alone decides.
	q := newQueue(pool, Options{}, goType, pyExplicit, unrouted)
	want := enqueue(t, pool, goType, nil, EnqueueOptions{})
	py1 := enqueue(t, pool, pyExplicit, nil, EnqueueOptions{Priority: 9})
	py2 := enqueue(t, pool, unrouted, nil, EnqueueOptions{Priority: 9})
	job := claim(t, q)
	if job == nil || job.ID != want {
		t.Fatalf("claimed %+v, want only %d", job, want)
	}
	if extra := claim(t, q); extra != nil {
		t.Fatalf("Go claimed Python-routed job %d", extra.ID)
	}
	for _, id := range []int64{py1, py2} {
		if r := getJob(t, pool, id); r.Status != "pending" {
			t.Fatalf("python job %d: %+v", id, r)
		}
	}
	// Unregistered Go-routed types are not claimed either.
	other := tag(t)
	routeGo(t, pool, other)
	enqueue(t, pool, other, nil, EnqueueOptions{Priority: 9})
	if extra := claim(t, q); extra != nil {
		t.Fatalf("claimed unregistered type: %+v", extra)
	}
}

func TestShutdownDrainsThenLeavesInterruptedJobs(t *testing.T) {
	pool := appPool(t)
	quick, stuck := tag(t), tag(t)
	routeGo(t, pool, quick, stuck)
	q := newQueue(pool, Options{Concurrency: 2, ShutdownGrace: 500 * time.Millisecond, IdlePoll: 20 * time.Millisecond})
	started := make(chan string, 2)
	stuckCause := make(chan error, 1)
	q.Register(quick, func(ctx context.Context, job Job) error {
		started <- quick
		time.Sleep(200 * time.Millisecond) // finishes inside the grace period
		return nil
	})
	q.Register(stuck, func(ctx context.Context, job Job) error {
		started <- stuck
		<-ctx.Done()
		stuckCause <- context.Cause(ctx)
		return ctx.Err()
	})
	quickID := enqueue(t, pool, quick, nil, EnqueueOptions{})
	stuckID := enqueue(t, pool, stuck, nil, EnqueueOptions{})

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- q.Run(ctx) }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("jobs did not start")
		}
	}
	shutdownAt := time.Now()
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}
	if elapsed := time.Since(shutdownAt); elapsed < 500*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("shutdown took %v; want grace (500ms) then prompt cancellation", elapsed)
	}
	if cause := <-stuckCause; !errors.Is(cause, errShutdown) {
		t.Fatalf("stuck handler cause = %v", cause)
	}
	if r := getJob(t, pool, quickID); r.Status != "completed" {
		t.Fatalf("job finishing within grace was not completed: %+v", r)
	}
	r := getJob(t, pool, stuckID)
	if r.Status != "processing" || str(r.WorkerID) != q.WorkerID() || r.Error != nil {
		t.Fatalf("interrupted job must be left for heartbeat recovery: %+v", r)
	}
}

func TestMetricsShape(t *testing.T) {
	pool := appPool(t)
	jt := tag(t)
	routeGo(t, pool, jt)
	q := newQueue(pool, Options{Concurrency: 3, MaxActivePerWorkspace: 2}, jt)
	enqueue(t, pool, jt, nil, EnqueueOptions{})
	mustExec(t, pool, `UPDATE jobs SET created_at = LOCALTIMESTAMP - interval '10 days' WHERE type = $1`, jt)
	m, err := q.Metrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.WorkerID != q.WorkerID() || m.ConfiguredConcurrency != 3 || m.MaxActivePerWorkspace != 2 || m.LocalActiveSlots != 0 {
		t.Fatalf("metrics header: %+v", m)
	}
	if m.Counts["pending"] < 1 || m.ActiveByType[jt] != 1 || m.OldestPendingAgeSeconds < 10*24*3600 {
		t.Fatalf("metrics body: %+v", m)
	}
	raw, _ := json.Marshal(m)
	var keys map[string]any
	json.Unmarshal(raw, &keys)
	for _, k := range []string{"worker_id", "configured_concurrency", "max_active_per_workspace", "local_active_slots",
		"active_workers", "counts", "active_by_type", "oldest_pending_age_seconds", "observed_at"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("metrics JSON lacks %q", k)
		}
	}
}

func TestSetRouteRefusesInFlight(t *testing.T) {
	pool := appPool(t)
	jt := tag(t)
	cleanupTypes(t, pool, jt)
	ctx := context.Background()
	if _, err := SetRoute(ctx, pool, jt, "rust", false); err == nil {
		t.Fatal("invalid executor accepted")
	}
	route, err := SetRoute(ctx, pool, jt, ExecutorGo, false)
	if err != nil || route.Executor != ExecutorGo {
		t.Fatalf("SetRoute = %+v, %v", route, err)
	}
	q := newQueue(pool, Options{}, jt)
	enqueue(t, pool, jt, nil, EnqueueOptions{})
	enqueue(t, pool, jt, nil, EnqueueOptions{})
	claim(t, q)

	var inflight *InFlightError
	if _, err := SetRoute(ctx, pool, jt, ExecutorPython, false); !errors.As(err, &inflight) || inflight.Processing != 1 {
		t.Fatalf("expected InFlightError, got %v", err)
	}
	route, err = SetRoute(ctx, pool, jt, ExecutorPython, true)
	if err != nil || route.Executor != ExecutorPython || route.Processing != 1 || route.Pending != 1 {
		t.Fatalf("forced SetRoute = %+v, %v", route, err)
	}
	routes, err := ListRoutes(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(routes, func(r Route) bool { return r.JobType == jt })
	if i < 0 || routes[i].Executor != ExecutorPython {
		t.Fatalf("ListRoutes missing %s: %+v", jt, routes)
	}
	if !slices.ContainsFunc(routes, func(r Route) bool { return r.JobType == "plugin_run" && r.Executor == ExecutorGo }) {
		t.Fatal("seeded plugin_run route missing")
	}
}

// TestCrossLanguageClaims runs the real Python QueueService against the same
// database and proves each executor claims only the types routed to it.
func TestCrossLanguageClaims(t *testing.T) {
	pool := appPool(t)
	if _, err := exec2("uv", "--version"); err != nil {
		t.Skip("uv not available")
	}
	goType, pyType := tag(t), tag(t)
	routeGo(t, pool, goType)
	cleanupTypes(t, pool, pyType)
	prefix := tag(t)
	var goIDs, pyIDs []int64
	for i := range 3 {
		goIDs = append(goIDs, enqueue(t, pool, goType, nil, EnqueueOptions{FireKey: fmt.Sprintf("%s:g%d", prefix, i)}))
		pyIDs = append(pyIDs, enqueue(t, pool, pyType, nil, EnqueueOptions{FireKey: fmt.Sprintf("%s:p%d", prefix, i)}))
	}

	script := `
import json, sys
from apps.api.services.queue_service import QueueService
q = QueueService()
ids = []
while (job := q.claim_next_job(fire_key_prefix=sys.argv[1])) is not None:
    ids.append(job["id"])
print(json.dumps(ids))
`
	cmd := exec.Command("uv", "run", "--frozen", "python", "-c", script, prefix)
	cmd.Dir = dbtest.RepoRoot(t)
	cmd.Env = append(os.Environ(), "DATABASE_URL="+dbtest.SQLAlchemyURL(dbtest.AppURL(t, ownerURL)))
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("python claimer failed: %v\n%s", err, ee.Stderr)
		}
		t.Fatal(err)
	}
	var pyClaimed []int64
	lines := splitLines(string(out))
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &pyClaimed); err != nil {
		t.Fatalf("python output %q: %v", out, err)
	}

	q := newQueue(pool, Options{FireKeyPrefix: prefix}, goType, pyType)
	var goClaimed []int64
	for job := claim(t, q); job != nil; job = claim(t, q) {
		goClaimed = append(goClaimed, job.ID)
	}
	slices.Sort(pyClaimed)
	slices.Sort(goClaimed)
	if !slices.Equal(pyClaimed, pyIDs) {
		t.Errorf("python claimed %v, want its own %v", pyClaimed, pyIDs)
	}
	if !slices.Equal(goClaimed, goIDs) {
		t.Errorf("go claimed %v, want its own %v", goClaimed, goIDs)
	}
}

func exec2(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

func splitLines(s string) []string {
	return strings.Split(strings.TrimSpace(s), "\n")
}
