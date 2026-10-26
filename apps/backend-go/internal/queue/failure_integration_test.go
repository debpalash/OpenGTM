//go:build integration

package queue_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type recorder struct {
	mu sync.Mutex
	ns []queue.FailureNotice
}

func (r *recorder) hook(err error) queue.FailureHook {
	return func(_ context.Context, n queue.FailureNotice) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.ns = append(r.ns, n)
		return err
	}
}

func hookFor(t *testing.T, err error) *recorder {
	t.Helper()
	r := &recorder{}
	queue.RegisterFailureHook(queue.JobTypeRunWorkbook, r.hook(err))
	t.Cleanup(func() { queue.RegisterFailureHook(queue.JobTypeRunWorkbook, nil) })
	return r
}

func claimed(t *testing.T, pool *pgxpool.Pool, retry, max int) (*queue.Claim, int64) {
	t.Helper()
	id := insert(t, pool, queue.JobTypeRunWorkbook, "go", "ws-1", "pending", retry, max)
	if _, err := pool.Exec(context.Background(), `UPDATE jobs SET payload='{"k":"v"}' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	return mustClaim(t, pool, "w", 0), id
}

var failed = queue.Outcome{Status: "failed", Error: "boom"}

func TestFailureHookCalledAfterPersistedFailure(t *testing.T) {
	pool, ctx := setup(t)
	r := hookFor(t, nil)
	c, id := claimed(t, pool, 3, 3)
	persisted, err := queue.FinalizeAndNotify(ctx, pool, quiet, c, failed)
	if err != nil || !persisted {
		t.Fatalf("persisted=%v err=%v", persisted, err)
	}
	if len(r.ns) != 1 {
		t.Fatalf("hook calls = %d", len(r.ns))
	}
	n := r.ns[0]
	if n.JobID != id || n.JobType != queue.JobTypeRunWorkbook || n.WorkspaceID != "ws-1" || n.Reason != "boom" || n.WillRetry ||
		string(n.Payload) != `{"k": "v"}` && string(n.Payload) != `{"k":"v"}` {
		t.Fatalf("notice = %+v", n)
	}
	// Completed outcomes never call the hook.
	c2, _ := claimed(t, pool, 0, 3)
	if _, err := queue.FinalizeAndNotify(ctx, pool, quiet, c2, queue.Outcome{Status: "completed"}); err != nil || len(r.ns) != 1 {
		t.Fatalf("hook ran for a completed job: %v %d", err, len(r.ns))
	}
}

func TestFailureHookReportsWillRetry(t *testing.T) {
	pool, ctx := setup(t)
	r := hookFor(t, nil)
	c, _ := claimed(t, pool, 0, 3)
	if _, err := queue.FinalizeAndNotify(ctx, pool, quiet, c, failed); err != nil {
		t.Fatal(err)
	}
	if len(r.ns) != 1 || !r.ns[0].WillRetry {
		t.Fatalf("retry notice = %+v", r.ns)
	}
	// Python substitutes a reason for an empty error.
	c2, _ := claimed(t, pool, 3, 3)
	if _, err := queue.FinalizeAndNotify(ctx, pool, quiet, c2, queue.Outcome{Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	if len(r.ns) != 2 || r.ns[1].WillRetry || r.ns[1].Reason != "job attempt failed" {
		t.Fatalf("final notice = %+v", r.ns)
	}
}

func TestFailureHookNotCalledOnLostLease(t *testing.T) {
	pool, ctx := setup(t)
	r := hookFor(t, nil)
	c, id := claimed(t, pool, 0, 3)
	if _, err := pool.Exec(ctx, `UPDATE jobs SET lease_token = lease_token + 1 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	persisted, err := queue.FinalizeAndNotify(ctx, pool, quiet, c, failed)
	if err != nil || persisted || len(r.ns) != 0 {
		t.Fatalf("persisted=%v err=%v hook calls=%d", persisted, err, len(r.ns))
	}
}

func TestFailureHookNotCalledOnCancelled(t *testing.T) {
	pool, ctx := setup(t)
	r := hookFor(t, nil)
	c, id := claimed(t, pool, 0, 3)
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	persisted, err := queue.FinalizeAndNotify(ctx, pool, quiet, c, failed)
	if err != nil || persisted || len(r.ns) != 0 {
		t.Fatalf("persisted=%v err=%v hook calls=%d", persisted, err, len(r.ns))
	}
}

func TestFailureHookErrorIsSwallowed(t *testing.T) {
	pool, ctx := setup(t)
	for name, h := range map[string]queue.FailureHook{
		"error": func(context.Context, queue.FailureNotice) error { return errors.New("hook broke") },
		"panic": func(context.Context, queue.FailureNotice) error { panic("hook panic") },
	} {
		queue.RegisterFailureHook(queue.JobTypeRunWorkbook, h)
		c, id := claimed(t, pool, 3, 3)
		persisted, err := queue.FinalizeAndNotify(ctx, pool, quiet, c, failed)
		if err != nil || !persisted {
			t.Fatalf("%s: persisted=%v err=%v", name, persisted, err)
		}
		var st string
		if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, id).Scan(&st); err != nil || st != "failed" {
			t.Fatalf("%s: status=%q err=%v", name, st, err)
		}
	}
	queue.RegisterFailureHook(queue.JobTypeRunWorkbook, nil)
}

func TestFinalizeSignatureUnchanged(t *testing.T) {
	var _ func(context.Context, *pgxpool.Pool, *queue.Claim, queue.Outcome) (bool, error) = queue.Finalize
	var _ func(context.Context, *pgxpool.Pool, *queue.Claim, queue.Outcome) (bool, bool, error) = queue.FinalizeNotify
	var _ func(string, queue.FailureHook) = queue.RegisterFailureHook
}

func TestFailureHookGetsFreshContext(t *testing.T) {
	pool, ctx := setup(t)
	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var got error
	var hasDeadline bool
	queue.RegisterFailureHook(queue.JobTypeRunWorkbook, func(hc context.Context, _ queue.FailureNotice) error {
		cancel() // the Finalize context dies; the hook context must not
		got = hc.Err()
		_, hasDeadline = hc.Deadline()
		return nil
	})
	t.Cleanup(func() { queue.RegisterFailureHook(queue.JobTypeRunWorkbook, nil) })
	c, _ := claimed(t, pool, 3, 3)
	if _, err := queue.FinalizeAndNotify(fctx, pool, quiet, c, failed); err != nil {
		t.Fatal(err)
	}
	if got != nil || !hasDeadline {
		t.Fatalf("hook ctx err=%v hasDeadline=%v", got, hasDeadline)
	}
}
