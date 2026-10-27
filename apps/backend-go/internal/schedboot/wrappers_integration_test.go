//go:build integration

package schedboot_test

import (
	"context"
	"testing"

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
