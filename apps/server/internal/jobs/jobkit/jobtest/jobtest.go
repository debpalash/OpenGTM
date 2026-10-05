// Package jobtest holds the PostgreSQL fixtures shared by the integration
// tests of the Go job executors: a migrated database, an owner pool (seeds
// and inspects, bypasses row-level security) and a runtime pool under a
// NOSUPERUSER NOBYPASSRLS role, unique workspace ids with cleanup, claimed and
// queued jobs, executor routing and a running queue. Only test code imports
// it.
package jobtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// Env is a migrated test database with an owner and a runtime pool.
type Env struct {
	T     *testing.T
	Owner *pgxpool.Pool // schema owner: bypasses RLS
	App   *pgxpool.Pool // NOSUPERUSER NOBYPASSRLS runtime role: what workers use
}

// New skips the test unless OPENGTM_TEST_DATABASE_URL is set, migrates the
// database and opens both pools.
func New(t *testing.T) *Env {
	t.Helper()
	ownerURL := dbtest.OwnerURL(t)
	dbtest.Migrate(t, ownerURL)
	return &Env{
		T:     t,
		Owner: dbtest.Pool(t, ownerURL, 4),
		App:   dbtest.Pool(t, dbtest.AppURL(t, ownerURL), 8),
	}
}

// Unique returns label plus random hex, safe to use as an identifier that
// cannot collide with another test or run sharing the database.
func Unique(label string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return label + "-" + hex.EncodeToString(b[:])
}

// Exec runs a statement as the schema owner and fails the test on error.
func (e *Env) Exec(sql string, args ...any) {
	e.T.Helper()
	if _, err := e.Owner.Exec(context.Background(), sql, args...); err != nil {
		e.T.Fatalf("%s: %v", sql, err)
	}
}

// Scalar returns the first column of the first row as text, "<nil>" for NULL.
func (e *Env) Scalar(sql string, args ...any) string {
	e.T.Helper()
	var v *string
	if err := e.Owner.QueryRow(context.Background(), "SELECT ("+sql+")::text", args...).Scan(&v); err != nil {
		e.T.Fatalf("%s: %v", sql, err)
	}
	if v == nil {
		return "<nil>"
	}
	return *v
}

// Count returns count(*) of table, optionally filtered by a WHERE clause.
func (e *Env) Count(table, where string, args ...any) int {
	e.T.Helper()
	sql := "SELECT count(*) FROM " + table
	if where != "" {
		sql += " WHERE " + where
	}
	var n int
	if err := e.Owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.T.Fatalf("%s: %v", sql, err)
	}
	return n
}

// Quote is a SQL string literal for s (for seed scripts that take no arguments).
func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// Claimed inserts a processing job holding a lease and returns it as the queue
// would hand it to the handler.
func (e *Env) Claimed(jobType, workspaceID string, payload any) queue.Job {
	e.T.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		e.T.Fatal(err)
	}
	j := queue.Job{Type: jobType, Payload: raw, WorkspaceID: workspaceID, Lease: queue.Lease{WorkerID: "test-worker"}}
	var ws *string
	if workspaceID != "" {
		ws = &workspaceID
	}
	err = e.Owner.QueryRow(context.Background(), `INSERT INTO jobs
		(type, payload, workspace_id, priority, status, created_at, next_run_at, max_retries, retry_count, worker_id, locked_at)
		VALUES ($1, $2::json, $3, 1, 'processing', LOCALTIMESTAMP, LOCALTIMESTAMP, 3, 0, 'test-worker', LOCALTIMESTAMP)
		RETURNING id, locked_at`, jobType, string(raw), ws).Scan(&j.ID, &j.Lease.LockedAt)
	if err != nil {
		e.T.Fatal(err)
	}
	return j
}

// Cancel marks a claimed job cancelled, as the API does, so its lease is lost.
func (e *Env) Cancel(job queue.Job) {
	e.T.Helper()
	e.Exec(`UPDATE jobs SET status = 'cancelled' WHERE id = $1`, job.ID)
}

// Queued inserts a pending job due now.
func (e *Env) Queued(jobType, workspaceID string, payload any) int64 {
	e.T.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		e.T.Fatal(err)
	}
	var id int64
	err = e.Owner.QueryRow(context.Background(), `INSERT INTO jobs
		(type, payload, workspace_id, priority, status, created_at, next_run_at, max_retries, retry_count)
		VALUES ($1, $2::json, $3, 1, 'pending', LOCALTIMESTAMP, LOCALTIMESTAMP, 3, 0) RETURNING id`,
		jobType, string(raw), workspaceID).Scan(&id)
	if err != nil {
		e.T.Fatal(err)
	}
	return id
}

// SetRoute routes jobType to executor and removes the route when the test ends.
func (e *Env) SetRoute(jobType, executor string) {
	e.T.Helper()
	e.Exec(`INSERT INTO job_executor_routes (job_type, executor) VALUES ($1, $2)
		ON CONFLICT (job_type) DO UPDATE SET executor = EXCLUDED.executor`, jobType, executor)
	e.T.Cleanup(func() {
		_, _ = e.Owner.Exec(context.Background(), `DELETE FROM job_executor_routes WHERE job_type = $1`, jobType)
	})
}

// ClearRoute removes any route for jobType (the default: Python-owned).
func (e *Env) ClearRoute(jobType string) {
	e.T.Helper()
	e.Exec(`DELETE FROM job_executor_routes WHERE job_type = $1`, jobType)
}

// StartQueue builds the registry from the registrars linked into the test
// binary, requires it to contribute exactly wantTypes, and runs a fast queue
// until the test ends.
func (e *Env) StartQueue(wantTypes ...string) {
	e.T.Helper()
	reg, err := queue.Build(queue.Env{Pool: e.App})
	if err != nil {
		e.T.Fatal(err)
	}
	slices.Sort(wantTypes)
	if !slices.Equal(reg.Types(), wantTypes) {
		e.T.Fatalf("registered types = %v; the registrar must contribute exactly %v", reg.Types(), wantTypes)
	}
	q := queue.New(e.App, reg, queue.Options{
		Concurrency: 1, IdlePoll: 20 * time.Millisecond, ClaimCheckInterval: 50 * time.Millisecond,
		ReapInterval: time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = q.Run(ctx) }()
	e.T.Cleanup(func() { cancel(); <-done })
}

// Eventually polls cond until it holds or 20 seconds pass.
func (e *Env) Eventually(what string, cond func() bool) {
	e.T.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.T.Fatalf("timed out waiting for %s", what)
}

// Switchable reports the executor `routes list` would show for jobType:
// "python (default)" for a declared type without a route.
func (e *Env) Switchable(jobType string) (executor string, isDefault bool) {
	e.T.Helper()
	routes, err := queue.ListRoutes(context.Background(), e.App)
	if err != nil {
		e.T.Fatal(err)
	}
	for _, r := range routes {
		if r.JobType == jobType {
			return r.Executor, r.Default
		}
	}
	e.T.Fatalf("%s is not listed by routes list; it must be declared switchable", jobType)
	return "", false
}

// RoutingScenario is the shared routing proof for a Python-owned-by-default
// job type: it is listed as a switchable python default, a pending job is
// never claimed while unrouted, the job is claimed and its effect observed
// once routed to go (done reports it), and a job enqueued after routing back
// to python is left alone. enqueue creates a pending job and returns its id.
func (e *Env) RoutingScenario(jobType string, enqueue func() int64, done func(jobID int64) bool) {
	e.T.Helper()
	e.ClearRoute(jobType)
	e.T.Cleanup(func() { e.ClearRoute(jobType) })
	if ex, def := e.Switchable(jobType); ex != "python" || !def {
		e.T.Fatalf("%s must be a switchable python default, got %s default=%v", jobType, ex, def)
	}
	first := enqueue()
	e.StartQueue(jobType)
	time.Sleep(400 * time.Millisecond)
	if st := e.Scalar(`SELECT status FROM jobs WHERE id = $1`, first); st != "pending" {
		e.T.Fatalf("an unrouted job was claimed by the go worker: %s", st)
	}
	e.SetRoute(jobType, "go")
	e.Eventually("the go worker to complete the job", func() bool {
		return e.Scalar(`SELECT status FROM jobs WHERE id = $1`, first) == "completed"
	})
	if !done(first) {
		e.T.Error("the job completed but its effect is missing")
	}
	e.SetRoute(jobType, "python")
	second := enqueue()
	time.Sleep(400 * time.Millisecond)
	if st := e.Scalar(`SELECT status FROM jobs WHERE id = $1`, second); st != "pending" {
		e.T.Fatalf("after rollback the go worker still claimed the job: %s", st)
	}
}
