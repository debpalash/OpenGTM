package jobkit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/server/internal/db"
	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

func suffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func TestMirrorUpsertWritesOnlyWhenSomethingChanged(t *testing.T) {
	ownerURL := dbtest.OwnerURL(t)
	dbtest.Migrate(t, ownerURL)
	owner := dbtest.Pool(t, ownerURL, 2)
	app := dbtest.Pool(t, dbtest.AppURL(t, ownerURL), 2)
	ctx := context.Background()
	key, ws := "jk-pb-"+suffix(), "jk-ws-"+suffix()
	t.Cleanup(func() { _, _ = owner.Exec(ctx, `DELETE FROM playbook_schedules WHERE playbook_id = $1`, key) })
	m := Mirror{Table: "playbook_schedules", KeyCol: "playbook_id", WorkspaceCol: "workspace_id", EnabledCol: "enabled", NextCol: "next_run_at"}
	next := time.Date(2026, 6, 15, 13, 0, 0, 0, time.UTC)

	upsert := func(enabled bool, n *time.Time) {
		t.Helper()
		if err := db.WithoutTenant(ctx, app, func(tx pgx.Tx) error { return m.Upsert(ctx, tx, key, ws, enabled, n) }); err != nil {
			t.Fatal(err)
		}
	}
	scalar := func(sql string) string {
		t.Helper()
		var s *string
		if err := owner.QueryRow(ctx, "SELECT ("+sql+")::text FROM playbook_schedules WHERE playbook_id = $1", key).Scan(&s); err != nil {
			t.Fatal(err)
		}
		if s == nil {
			return "<nil>"
		}
		return *s
	}

	upsert(true, &next)
	if scalar(`enabled`) != "true" || scalar(`workspace_id`) != ws || scalar(`next_run_at = '2026-06-15 13:00:00+00'::timestamptz::timestamp`) != "true" {
		t.Fatalf("insert did not store the row")
	}
	if _, err := owner.Exec(ctx, `UPDATE playbook_schedules SET updated_at = '2001-01-01' WHERE playbook_id = $1`, key); err != nil {
		t.Fatal(err)
	}
	upsert(true, &next) // identical: must not touch the row
	if got := scalar(`updated_at::date`); got != "2001-01-01" {
		t.Errorf("a no-op upsert bumped updated_at to %s", got)
	}
	upsert(false, nil) // change
	if scalar(`enabled`) != "false" || scalar(`next_run_at`) != "<nil>" || scalar(`updated_at::date`) == "2001-01-01" {
		t.Errorf("a changed upsert was not written: enabled=%s next=%s", scalar(`enabled`), scalar(`next_run_at`))
	}
	if err := db.WithoutTenant(ctx, app, func(tx pgx.Tx) error { return m.Delete(ctx, tx, key) }); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM playbook_schedules WHERE playbook_id = $1`, key).Scan(&n); err != nil || n != 0 {
		t.Errorf("Delete left %d rows (%v)", n, err)
	}
}

func TestCancelPendingUsesUnescapedLikeAndSkipsOtherStates(t *testing.T) {
	ownerURL := dbtest.OwnerURL(t)
	dbtest.Migrate(t, ownerURL)
	owner := dbtest.Pool(t, ownerURL, 2)
	app := dbtest.Pool(t, dbtest.AppURL(t, ownerURL), 2)
	ctx := context.Background()
	typ := "jk_cancel_" + suffix()
	t.Cleanup(func() { _, _ = owner.Exec(ctx, `DELETE FROM jobs WHERE type = $1`, typ) })
	add := func(status, key string) {
		t.Helper()
		if _, err := owner.Exec(ctx, `INSERT INTO jobs (type, payload, priority, status, fire_key, created_at, next_run_at, max_retries, retry_count)
			VALUES ($1, '{}', 1, $2, $3, LOCALTIMESTAMP, LOCALTIMESTAMP, 3, 0)`, typ, status, key); err != nil {
			t.Fatal(err)
		}
	}
	add("pending", "x:a_b:1")
	add("pending", "x:aXb:2") // '_' is a wildcard, exactly as in Python
	add("processing", "x:a_b:3")
	add("pending", "y:a_b:4")
	var n int64
	err := db.WithoutTenant(ctx, app, func(tx pgx.Tx) error {
		var err error
		n, err = CancelPending(ctx, tx, typ, "x:a_b:%")
		return err
	})
	if err != nil || n != 2 {
		t.Fatalf("cancelled %d, %v; want 2", n, err)
	}
	var cancelled, processing, pending int
	_ = owner.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='cancelled'), count(*) FILTER (WHERE status='processing'),
		count(*) FILTER (WHERE status='pending') FROM jobs WHERE type = $1`, typ).Scan(&cancelled, &processing, &pending)
	if cancelled != 2 || processing != 1 || pending != 1 {
		t.Errorf("cancelled=%d processing=%d pending=%d", cancelled, processing, pending)
	}
}

func TestWithLeaseRefusesAStaleAttempt(t *testing.T) {
	ownerURL := dbtest.OwnerURL(t)
	dbtest.Migrate(t, ownerURL)
	owner := dbtest.Pool(t, ownerURL, 2)
	app := dbtest.Pool(t, dbtest.AppURL(t, ownerURL), 2)
	ctx := context.Background()
	typ := "jk_lease_" + suffix()
	t.Cleanup(func() { _, _ = owner.Exec(ctx, `DELETE FROM jobs WHERE type = $1`, typ) })
	job := queue.Job{Type: typ, Lease: queue.Lease{WorkerID: "jk-worker"}}
	if err := owner.QueryRow(ctx, `INSERT INTO jobs (type, payload, priority, status, created_at, next_run_at, max_retries, retry_count, worker_id, locked_at)
		VALUES ($1, '{}', 1, 'processing', LOCALTIMESTAMP, LOCALTIMESTAMP, 3, 0, 'jk-worker', LOCALTIMESTAMP) RETURNING id, locked_at`, typ).
		Scan(&job.ID, &job.Lease.LockedAt); err != nil {
		t.Fatal(err)
	}
	ran := false
	if err := WithLease(ctx, app, "jk-ws", job, func(pgx.Tx) error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("a live lease must run the body: %v", err)
	}
	if _, err := owner.Exec(ctx, `UPDATE jobs SET status = 'cancelled' WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}
	ran = false
	err := WithLease(ctx, app, "jk-ws", job, func(pgx.Tx) error { ran = true; return nil })
	if !errors.Is(err, queue.ErrLeaseLost) || ran {
		t.Errorf("cancelled lease: err=%v ran=%v", err, ran)
	}
	if err := WithLease(ctx, app, " ", job, func(pgx.Tx) error { return nil }); !errors.Is(err, db.ErrNoWorkspace) {
		t.Errorf("an empty workspace must fail closed: %v", err)
	}
}
