//go:build integration

package schedboot_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func TestSourceHealthBootstrapGatedAndIdempotent(t *testing.T) {
	pool := testpg.NewAppPool(t, 8)
	own := owner(t)
	ctx := context.Background()
	seed(t, own)
	d := deps(pool, "python")
	count := func() (n int) {
		if err := own.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE type = 'source_health_check'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	d.Getenv = func(string) string { return "" }
	if got, err := d.SourceHealth(ctx); err != nil || got || count() != 0 {
		t.Fatalf("probe off must enqueue nothing: %v %v %d", got, err, count())
	}
	d.Getenv = func(k string) string {
		if k == "SOURCE_HEALTH_ENABLED" {
			return "true"
		}
		return ""
	}
	if got, err := d.SourceHealth(ctx); err != nil || !got || count() != 1 {
		t.Fatalf("first bootstrap: %v %v %d", got, err, count())
	}
	if got, err := d.SourceHealth(ctx); err != nil || got || count() != 1 {
		t.Fatalf("second bootstrap must be a no-op: %v %v %d", got, err, count())
	}
}

func TestAutomationsAndPollerBootstrapsOffByFlag(t *testing.T) {
	pool := testpg.NewAppPool(t, 8)
	ctx := context.Background()
	d := deps(pool, "python_flags_off") // AUTOMATIONS_ENABLED off, INTENT_POLLER_ENABLED off
	if n, err := d.Automations(ctx); err != nil || n != 0 {
		t.Fatalf("automations: %d %v", n, err)
	}
	if n, err := d.IntentPoller(ctx); err != nil || n != 0 {
		t.Fatalf("poller: %d %v", n, err)
	}
}

// M3: two reconcilers racing on signal_scan enqueue exactly one job (pending check and insert are one
// transaction under an advisory lock; the fire key is time-based so the unique index alone cannot stop it).
func TestSignalScanConcurrentPassesEnqueueOnce(t *testing.T) {
	pool := testpg.NewAppPool(t, 16)
	own := owner(t)
	ctx := context.Background()
	for round := 0; round < 5; round++ {
		if _, err := own.Exec(ctx, `DELETE FROM jobs WHERE type = 'signal_scan'`); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				d := deps(pool, "python")
				d.Now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC) }
				if _, err := d.SignalScan(ctx); err != nil {
					t.Error(err)
				}
			}(i)
		}
		wg.Wait()
		var n int
		if err := own.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE type = 'signal_scan'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("round %d: %d signal_scan jobs, want 1", round, n)
		}
	}
	_, _ = own.Exec(ctx, `DELETE FROM jobs WHERE type = 'signal_scan'`)
}
