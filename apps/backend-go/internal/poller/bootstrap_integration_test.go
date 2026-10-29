//go:build integration

package poller_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/poller"
)

// bootstrap_watch_schedules: the non-RLS mirror names due watches; each is re-read inside its own
// workspace and enqueued single-flight with the executor the cohort picks.
func TestBootstrapWatchSchedules(t *testing.T) {
	pool, ctx := setup(t)
	for id, enabled := range map[string]bool{wID: true, pfx + "off": false, pfx + "later": true} {
		inTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO watch_subscriptions (id, workspace_id, kind, target, "interval", schedule_anchor, enabled, cursor)
				VALUES ($1,$2,'funding',$1,'daily',$3,$4,'{"bootstrapped": false}'::json)`, id, wsA, vAnchor, enabled)
			return err
		})
	}
	future := vNow.Add(3 * time.Hour)
	inTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		if err := poller.MirrorSet(ctx, tx, wID, wsA, nil, true); err != nil { // NULL next: due
			return err
		}
		if err := poller.MirrorSet(ctx, tx, pfx+"off", wsA, nil, true); err != nil { // watch disabled
			return err
		}
		if err := poller.MirrorSet(ctx, tx, pfx+"later", wsA, &future, true); err != nil { // not due
			return err
		}
		return poller.MirrorSet(ctx, tx, pfx+"ghost", wsA, nil, true) // no such watch
	})
	// A mirror row naming A's watch under workspace B must not touch A's watch.
	inTx(t, ctx, pool, wsB, func(tx pgx.Tx) error { return poller.MirrorSet(ctx, tx, pfx+"later", wsB, nil, true) })

	exec := func(string) string { return "go" }
	n, err := poller.BootstrapWatchSchedules(ctx, pool, vNow, cfg, exec)
	if err != nil || n != 1 {
		t.Fatalf("enqueued %d err %v", n, err)
	}
	js := jobs(t, ctx, pool, "watch:"+pfx+"%")
	if len(js) != 1 || js[0].Executor != "go" || js[0].Type != "watch_poll" {
		t.Fatalf("jobs %+v", js)
	}
	wantNext := poller.NextPollFor(poller.Watch{Anchor: &vAnchor, Interval: "daily"}, vNow, cfg)
	if !js[0].NextRunAt.Equal(wantNext) {
		t.Fatalf("next_run_at %v want %v", js[0].NextRunAt, wantNext)
	}
	var mirrorNext time.Time
	var watchNext *time.Time
	if err := pool.QueryRow(ctx, `SELECT next_poll_at FROM watch_schedules WHERE watch_id=$1`, wID).Scan(&mirrorNext); err != nil || !mirrorNext.Equal(wantNext) {
		t.Fatalf("mirror next %v err %v", mirrorNext, err)
	}
	inTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT next_poll_at FROM watch_subscriptions WHERE id=$1`, wID).Scan(&watchNext)
	})
	if watchNext == nil || !watchNext.Equal(wantNext) {
		t.Fatalf("watch next_poll_at %v", watchNext)
	}
	// Idempotent: the mirror is no longer due and the active job holds the fire key.
	if n, err := poller.BootstrapWatchSchedules(ctx, pool, vNow, cfg, exec); err != nil || n != 0 {
		t.Fatalf("second pass %d %v", n, err)
	}
	if len(jobs(t, ctx, pool, "watch:"+pfx+"%")) != 1 {
		t.Fatal("duplicate job")
	}
}

func TestBootstrapWatchSchedulesIfGates(t *testing.T) {
	pool, ctx := setup(t)
	seed{ID: wID, WS: wsA, Interval: sp("daily"), Anchor: &vAnchor, Enabled: true}.insert(t, ctx, pool)
	inTx(t, ctx, pool, wsA, func(tx pgx.Tx) error { return poller.MirrorSet(ctx, tx, wID, wsA, nil, true) })
	for _, c := range []struct{ enabled, pg bool }{{false, true}, {true, false}} {
		if n, err := poller.BootstrapWatchSchedulesIf(ctx, pool, c.enabled, c.pg, vNow, cfg, nil); err != nil || n != 0 {
			t.Fatalf("%+v: %d %v", c, n, err)
		}
	}
	if len(jobs(t, ctx, pool, "watch:"+pfx+"%")) != 0 {
		t.Fatal("gated bootstrap enqueued")
	}
	if n, err := poller.BootstrapWatchSchedulesIf(ctx, pool, true, true, vNow, cfg, nil); err != nil || n != 1 {
		t.Fatalf("enabled: %d %v", n, err)
	}
	if js := jobs(t, ctx, pool, "watch:"+pfx+"%"); len(js) != 1 || js[0].Executor != "go" {
		t.Fatalf("nil execFor must be go: %+v", js)
	}
}
