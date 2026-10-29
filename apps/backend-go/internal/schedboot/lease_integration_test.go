//go:build integration

package schedboot_test

import (
	"context"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/schedboot"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func TestLeaseOneActiveScheduler(t *testing.T) {
	pool := testpg.NewAppPool(t, 8)
	ctx := context.Background()
	a, b := schedboot.NewLease(pool), schedboot.NewLease(pool)
	defer a.Release(ctx)
	defer b.Release(ctx)
	if ok, err := a.Hold(ctx); err != nil || !ok {
		t.Fatalf("first instance: %v %v", ok, err)
	}
	if ok, err := b.Hold(ctx); err != nil || ok {
		t.Fatalf("second instance must idle: %v %v", ok, err)
	}
	if ok, err := a.Hold(ctx); err != nil || !ok {
		t.Fatalf("the holder keeps the lease on later cycles: %v %v", ok, err)
	}
	a.Release(ctx)
	if ok, err := b.Hold(ctx); err != nil || !ok {
		t.Fatalf("after release the standby takes over: %v %v", ok, err)
	}
}

func TestLeaseLostConnectionFailsOverNotSplitBrain(t *testing.T) {
	pool := testpg.NewAppPool(t, 8)
	own := owner(t)
	ctx := context.Background()
	a, b := schedboot.NewLease(pool), schedboot.NewLease(pool)
	defer a.Release(ctx)
	defer b.Release(ctx)
	if ok, _ := a.Hold(ctx); !ok {
		t.Fatal("a should hold")
	}
	// kill the holder's backend: Postgres drops its session lock
	if _, err := own.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype='advisory' AND granted AND objid = $1::bigint::oid`, int64(schedboot.LeaseKey)); err != nil {
		t.Fatal(err)
	}
	// b may now take it; a must report either true (re-acquired first) or false, never both true
	okB, _ := b.Hold(ctx)
	okA, _ := a.Hold(ctx)
	if okA && okB {
		t.Fatal("two active schedulers")
	}
	if !okA && !okB {
		t.Fatal("lock must be re-acquirable after the holder's connection died")
	}
}
