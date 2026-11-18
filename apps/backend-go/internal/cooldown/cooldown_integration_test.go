//go:build integration

package cooldown_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/cooldown"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// ahead is the seconds until cooldown_until, -1 when NULL.
func ahead(t *testing.T, q func(string, ...any) pgx.Row, provider, field string) float64 {
	t.Helper()
	var s float64
	if err := q(`SELECT COALESCE(EXTRACT(EPOCH FROM (cooldown_until - (now() AT TIME ZONE 'UTC'))), -1)::float8
	  FROM provider_stats WHERE provider=$1 AND field=$2`, provider, field).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTimeoutPolicy(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	ctx := context.Background()
	prov := fmt.Sprintf("t20_cd_%d", os.Getpid())
	_, _ = pool.Exec(ctx, `DELETE FROM provider_stats WHERE provider LIKE 't20\_cd\_%'`)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM provider_stats WHERE provider LIKE 't20\_cd\_%'`) })
	field := "email"
	rec := func(a cooldown.Attempt) {
		t.Helper()
		a.Provider, a.Field = prov, field
		err := tenancy.InWorkspace(ctx, pool, "ws1", func(tx pgx.Tx) error { return cooldown.Record(ctx, gen.New(tx), a) })
		if err != nil {
			t.Fatal(err)
		}
	}
	cd := func() float64 {
		return ahead(t, func(sql string, args ...any) pgx.Row { return pool.QueryRow(ctx, sql, args...) }, prov, field)
	}
	timeout := cooldown.Attempt{TimedOut: true}

	rec(timeout)
	rec(timeout)
	if got := cd(); got != -1 {
		t.Fatalf("two timeouts must not cool the provider down, cooldown in %v s", got)
	}
	rec(timeout)
	if got := cd(); got < 150 || got > 181 {
		t.Fatalf("third consecutive timeout: want a ~180 s cooldown, got %v", got)
	}

	// A success or a miss resets the run of timeouts.
	for _, reset := range []cooldown.Attempt{{Success: true, Confidence: 0.9}, {}} {
		other := fmt.Sprintf("%s_%v", prov, reset.Success)
		recOther := func(a cooldown.Attempt) {
			a.Provider, a.Field = other, field
			if err := tenancy.InWorkspace(ctx, pool, "ws1", func(tx pgx.Tx) error { return cooldown.Record(ctx, gen.New(tx), a) }); err != nil {
				t.Fatal(err)
			}
		}
		recOther(timeout)
		recOther(timeout)
		recOther(reset)
		recOther(timeout)
		recOther(timeout)
		got := ahead(t, func(sql string, args ...any) pgx.Row { return pool.QueryRow(ctx, sql, args...) }, other, field)
		if got != -1 {
			t.Fatalf("a reset (success=%v) must restart the count, but the provider cooled down (%v s)", reset.Success, got)
		}
	}
}

func TestRateLimitCoolsDownAtOnce(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	ctx := context.Background()
	prov := fmt.Sprintf("t20_cd_rl_%d", os.Getpid())
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM provider_stats WHERE provider LIKE 't20\_cd\_%'`) })
	err := tenancy.InWorkspace(ctx, pool, "ws1", func(tx pgx.Tx) error {
		return cooldown.Record(ctx, gen.New(tx), cooldown.Attempt{Provider: prov, Field: "phone", RateLimited: true})
	})
	if err != nil {
		t.Fatal(err)
	}
	var s float64
	if err := pool.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM (cooldown_until - (now() AT TIME ZONE 'UTC')))::float8
	  FROM provider_stats WHERE provider=$1`, prov).Scan(&s); err != nil {
		t.Fatal(err)
	}
	if s < 270 || s > 301 {
		t.Fatalf("rate limit cooldown %v s, want ~300", s)
	}
}
