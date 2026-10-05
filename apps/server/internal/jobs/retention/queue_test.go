package retention

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

func startQueue(t *testing.T, e *env) {
	t.Helper()
	reg, err := queue.Build(queue.Env{Pool: e.app})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reg.Types(), []string{JobType}) {
		t.Fatalf("registered types = %v; the registrar must contribute exactly retention_enforce", reg.Types())
	}
	q := queue.New(e.app, reg, queue.Options{
		Concurrency: 1, IdlePoll: 20 * time.Millisecond, ClaimCheckInterval: 50 * time.Millisecond,
		ReapInterval: time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = q.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func (e *env) setRoute(executor string) {
	e.t.Helper()
	e.exec(`INSERT INTO job_executor_routes (job_type, executor) VALUES ($1, $2)
		ON CONFLICT (job_type) DO UPDATE SET executor = EXCLUDED.executor`, JobType, executor)
}

func (e *env) eventually(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for %s", what)
}

func (e *env) enqueuePending(ws, runID string) int64 {
	e.t.Helper()
	var id int64
	err := e.owner.QueryRow(context.Background(), `INSERT INTO jobs
		(type, payload, workspace_id, priority, status, created_at, next_run_at, max_retries, retry_count)
		VALUES ('retention_enforce', json_build_object('workspace_id', $1::text, 'run_id', $2::text), $1, 1, 'pending',
		        LOCALTIMESTAMP, LOCALTIMESTAMP, 3, 0) RETURNING id`, ws, runID).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// The type stays Python-owned until routed: listed as switchable, never
// claimed, then claimed and completed once routed to go, and handed back by
// routing it to python.
func TestRoutingControlsWhoClaimsRetentionEnforce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, _ = e.owner.Exec(ctx, `DELETE FROM job_executor_routes WHERE job_type = $1`, JobType)
	t.Cleanup(func() { _, _ = e.owner.Exec(ctx, `DELETE FROM job_executor_routes WHERE job_type = $1`, JobType) })

	routes, err := queue.ListRoutes(ctx, e.app)
	if err != nil {
		t.Fatal(err)
	}
	var found *queue.Route
	for i := range routes {
		if routes[i].JobType == JobType {
			found = &routes[i]
		}
	}
	if found == nil || found.Executor != "python" || !found.Default {
		t.Fatalf("retention_enforce must be listed as a switchable python default, got %+v", found)
	}

	ws := e.ws("route")
	e.seed(ws)
	e.policy(ws, true, false, defaultPolicyJSON)
	e.run(ws, "run-"+ws, "1", defaultPolicyJSON)
	jobID := e.enqueuePending(ws, "run-"+ws)
	startQueue(t, e)

	time.Sleep(400 * time.Millisecond)
	if st := e.scalar(`SELECT status FROM jobs WHERE id = $1`, jobID); st != "pending" {
		t.Fatalf("an unrouted job was claimed by the go worker: %s", st)
	}

	e.setRoute("go")
	e.eventually("the go worker to complete the job", func() bool {
		return e.scalar(`SELECT status FROM jobs WHERE id = $1`, jobID) == "completed"
	})
	if s, _, _, _ := e.runRow("run-" + ws); s != "completed" {
		t.Errorf("run = %s", s)
	}
	if e.scalar(`SELECT count(*) FROM jobs WHERE fire_key LIKE $1 AND status = 'pending'`, "retention:"+ws+":%") != "1" {
		t.Error("the completed job must have scheduled exactly one next run")
	}

	// Rollback: routed back to python, a new job is left for the Python worker.
	e.setRoute("python")
	ws2 := e.ws("route2")
	e.seed(ws2)
	e.policy(ws2, true, false, defaultPolicyJSON)
	e.run(ws2, "run-"+ws2, "1", defaultPolicyJSON)
	job2 := e.enqueuePending(ws2, "run-"+ws2)
	time.Sleep(400 * time.Millisecond)
	if st := e.scalar(`SELECT status FROM jobs WHERE id = $1`, job2); st != "pending" {
		t.Fatalf("after rollback the go worker still claimed the job: %s", st)
	}
}

// A failing attempt goes through the real queue: finalize schedules the
// retry, then the registered FailureHandler rewrites the run exactly as
// reconcile_retention_job_failure does.
func TestQueueRunsTheFailureReconciler(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.setRoute("go")
	t.Cleanup(func() { _, _ = e.owner.Exec(ctx, `DELETE FROM job_executor_routes WHERE job_type = $1`, JobType) })

	ws := e.ws("qfail")
	e.seed(ws)
	e.policy(ws, true, false, defaultPolicyJSON)
	e.run(ws, "run-"+ws, "1", defaultPolicyJSON)
	fn := "rt_qboom_" + strings.ReplaceAll(ws, "-", "_")
	e.exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF OLD.workspace_id = %s THEN RAISE EXCEPTION 'boom'; END IF; RETURN OLD; END $$`, fn, quote(ws)))
	e.exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE DELETE ON signals FOR EACH ROW EXECUTE FUNCTION %s()`, fn, fn))
	t.Cleanup(func() {
		_, _ = e.owner.Exec(ctx, fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON signals", fn))
		_, _ = e.owner.Exec(ctx, fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", fn))
	})

	jobID := e.enqueuePending(ws, "run-"+ws)
	startQueue(t, e)
	e.eventually("the failed attempt to be reconciled", func() bool {
		s, er, _, _ := e.runRow("run-" + ws)
		return s == "pending" && strings.HasPrefix(er, "Queue retry scheduled: ") && strings.Contains(er, "boom")
	})
	if e.scalar(`SELECT status FROM jobs WHERE id = $1`, jobID) != "pending" ||
		!strings.HasPrefix(e.scalar(`SELECT error FROM jobs WHERE id = $1`, jobID), "Retry 1: ") {
		t.Error("the queue must have scheduled a retry")
	}
	if n := e.count("governance_audit_events", ws); n != seeded("governance_audit_events") {
		t.Errorf("failed purge left a partial deletion: %d rows", n)
	}
}
