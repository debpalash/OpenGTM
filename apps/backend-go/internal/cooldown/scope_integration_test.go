//go:build integration

package cooldown_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/cooldown"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type ledger struct {
	t    *testing.T
	pool interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}
	rec  func(cooldown.Attempt)
	prov string
}

func newLedger(t *testing.T, tag string) *ledger {
	t.Helper()
	pool := testpg.NewAppPool(t, 2)
	ctx := context.Background()
	prov := fmt.Sprintf("t20_cd_%s_%d", tag, os.Getpid())
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM provider_stats WHERE provider LIKE 't20\_cd\_%'`) })
	l := &ledger{t: t, pool: pool, prov: prov}
	l.rec = func(a cooldown.Attempt) {
		a.Provider, a.Field = prov, "email"
		if err := tenancy.InWorkspace(ctx, pool, "ws1", func(tx pgx.Tx) error { return cooldown.Record(ctx, gen.New(tx), a) }); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

// secondsAhead is the seconds until cooldown_until, -1 when NULL.
func (l *ledger) secondsAhead() float64 {
	var s float64
	if err := l.pool.QueryRow(context.Background(), `SELECT COALESCE(EXTRACT(EPOCH FROM (cooldown_until - (now() AT TIME ZONE 'UTC'))), -1)::float8
	  FROM provider_stats WHERE provider=$1 AND field='email'`, l.prov).Scan(&s); err != nil {
		l.t.Fatal(err)
	}
	return s
}

func (l *ledger) streak() int {
	var n int
	if err := l.pool.QueryRow(context.Background(), `SELECT consecutive_timeouts FROM provider_stats WHERE provider=$1 AND field='email'`, l.prov).Scan(&n); err != nil {
		l.t.Fatal(err)
	}
	return n
}

// Twelve rows launched together that all time out are one event, not twelve: they all started before the first of
// them failed, so none of them is a second look at a provider already known to have failed.
func TestTimeoutsOfCallsStartedTogetherAreOneEvent(t *testing.T) {
	l := newLedger(t, "burst")
	t0 := time.Now().UTC()
	for i := 0; i < 12; i++ {
		l.rec(cooldown.Attempt{TimedOut: true, StartedAt: t0, FinishedAt: t0.Add(10*time.Second + time.Duration(i)*time.Millisecond)})
	}
	if got := l.secondsAhead(); got != -1 {
		t.Fatalf("12 simultaneous timeouts cooled the provider down for %v s", got)
	}
	if got := l.streak(); got != 1 {
		t.Fatalf("12 simultaneous timeouts counted as %d consecutive timeouts, want 1", got)
	}
}

// Three rounds of timeouts, each round started after the previous one failed and with no success between, is
// evidence of an unhealthy upstream.
func TestThreeSuccessiveRoundsOfTimeoutsCoolTheProviderDown(t *testing.T) {
	l := newLedger(t, "rounds")
	start := time.Now().UTC().Add(-time.Minute)
	for round := 0; round < 3; round++ {
		for i := 0; i < 4; i++ { // four rows launched together per round
			l.rec(cooldown.Attempt{TimedOut: true, StartedAt: start, FinishedAt: start.Add(10 * time.Second)})
		}
		if round < 2 && l.secondsAhead() != -1 {
			t.Fatalf("cooled down after only %d rounds", round+1)
		}
		start = start.Add(11 * time.Second)
	}
	if got := l.secondsAhead(); got < 150 || got > 181 {
		t.Fatalf("after three rounds want a ~180 s cooldown, got %v", got)
	}
}

// A success that finishes between two rounds resets the run, whichever round started first.
func TestSuccessBetweenRoundsResetsTheRun(t *testing.T) {
	l := newLedger(t, "reset")
	start := time.Now().UTC().Add(-time.Minute)
	l.rec(cooldown.Attempt{TimedOut: true, StartedAt: start, FinishedAt: start.Add(10 * time.Second)})
	l.rec(cooldown.Attempt{Success: true, Confidence: 0.9, StartedAt: start.Add(11 * time.Second), FinishedAt: start.Add(12 * time.Second)})
	l.rec(cooldown.Attempt{TimedOut: true, StartedAt: start.Add(13 * time.Second), FinishedAt: start.Add(23 * time.Second)})
	l.rec(cooldown.Attempt{TimedOut: true, StartedAt: start.Add(24 * time.Second), FinishedAt: start.Add(34 * time.Second)})
	if got := l.secondsAhead(); got != -1 {
		t.Fatalf("a success between the rounds must restart the count, cooldown in %v s", got)
	}
	if got := l.streak(); got != 2 {
		t.Fatalf("streak %d, want 2", got)
	}
}

// A timeout fetching one lead's own website says nothing about the next lead's, so it never cools the provider down.
func TestTargetSiteTimeoutsNeverCoolDown(t *testing.T) {
	l := newLedger(t, "target")
	start := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 10; i++ {
		l.rec(cooldown.Attempt{TimedOut: true, TargetSite: true, StartedAt: start, FinishedAt: start.Add(10 * time.Second)})
		start = start.Add(11 * time.Second)
	}
	if got := l.secondsAhead(); got != -1 {
		t.Fatalf("target-site timeouts cooled the provider down for %v s", got)
	}
}

// A rate limit still cools a target-site provider down at once (the browser service itself said 429).
func TestTargetSiteRateLimitStillCoolsDown(t *testing.T) {
	l := newLedger(t, "targetrl")
	l.rec(cooldown.Attempt{RateLimited: true, TargetSite: true})
	if got := l.secondsAhead(); got < 270 || got > 301 {
		t.Fatalf("want a 300 s cooldown, got %v", got)
	}
}
