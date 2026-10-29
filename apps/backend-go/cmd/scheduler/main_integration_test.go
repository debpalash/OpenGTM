//go:build integration

package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/schedboot"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func TestSecondInstanceIdlesWhileFirstReconciles(t *testing.T) {
	pool := testpg.NewAppPool(t, 12)
	own, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	ctx := context.Background()
	reset := func() { _, _ = own.Exec(ctx, `DELETE FROM jobs WHERE type = 'signal_scan'`) }
	reset()
	t.Cleanup(reset)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var cfg config.Config
	// the same Deps in both instances: only the holder of the lock may enqueue
	d := schedboot.Deps{Pool: pool, Cfg: cfg, Log: log}

	count := func() (n int) {
		_ = own.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE type = 'signal_scan'`).Scan(&n)
		return n
	}
	ctxA, stopA := context.WithCancel(ctx)
	doneA := make(chan error, 1)
	go func() { doneA <- serve(ctxA, pool, d, 20*time.Millisecond, log) }()
	waitFor(t, func() bool { return count() == 1 }, "the active scheduler enqueues the first signal_scan")

	// A is active; B must idle even after its jobs are deleted (a reconcile by B would re-create the scan)
	ctxB, stopB := context.WithCancel(ctx)
	defer stopB()
	doneB := make(chan error, 1)
	go func() { doneB <- serve(ctxB, pool, d, 20*time.Millisecond, log) }()
	time.Sleep(100 * time.Millisecond)
	stopA()
	<-doneA
	reset()
	// A released the lease on exit: B takes over and reconciles
	waitFor(t, func() bool { return count() == 1 }, "the standby takes over after the active scheduler stops")
	stopB()
	<-doneB
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", what)
}

func TestCheckRoleRefusesSuperuser(t *testing.T) {
	own, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Skip("TEST_DATABASE_URL not set")
	}
	defer own.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var su bool
	if err := own.QueryRow(context.Background(), `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&su); err != nil || !su {
		t.Skip("owner role is not a superuser")
	}
	if err := checkRole(context.Background(), own, true, log); err == nil {
		t.Fatal("a superuser role must be refused when strict")
	}
	if err := checkRole(context.Background(), own, false, log); err != nil {
		t.Fatalf("non-strict only warns: %v", err)
	}
}
