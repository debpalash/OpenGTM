//go:build integration

package collect

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectledger"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

func setLedger(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, id, status, msg string, attempts int) {
	t.Helper()
	if err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE collection_jobs SET status = $2, error = $3, attempts = $4, leads_found = 7
			WHERE id = $1`, id, status, msg, attempts)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// M2: a redelivered job whose ledger row is terminal (done, failed) or owned by a live run
// (running) is never re-run: no attempt bump, no stages, no events, no ledger change.
func TestCollectRedeliveryNeverReruns(t *testing.T) {
	pool, ctx := itSetup(t)
	spec, maps := jobNamed(t, "location_all_sources")
	for _, tc := range []struct{ status, msg string }{
		{"done", ""},
		{"failed", "Final failure: job timed out"},
		{"failed", "boom"},
		{"running", ""},
	} {
		t.Run(tc.status+"/"+tc.msg, func(t *testing.T) {
			em := &fakeEmitter{}
			d := itDeps(t, pool, spec, maps, em, nil)
			id := fmt.Sprintf("jr-%s-%d", tc.status, len(tc.msg))
			c := itJob(t, ctx, pool, itWS, id, spec.Spec.Query)
			setLedger(t, ctx, pool, itWS, id, tc.status, tc.msg, 1)
			if err := HandleCollect(ctx, d, c); err != nil {
				t.Fatalf("redelivery of a %s job: %v", tc.status, err)
			}
			j, stages := readJob(t, ctx, pool, itWS, id)
			if j.Status != tc.status || j.Error != tc.msg || j.Attempts != 1 || j.LeadsFound != 7 || len(stages) != 0 {
				t.Fatalf("job re-run or changed: %+v stages %v", j, stages)
			}
			if len(em.events) != 0 {
				t.Fatalf("events emitted on redelivery: %v", em.events)
			}
		})
	}
}

// M2: MarkRunning moves only a pending row; a cancel that lands before the run starts wins.
func TestCollectCancelBeforeStartWins(t *testing.T) {
	pool, ctx := itSetup(t)
	spec, maps := jobNamed(t, "location_all_sources")
	em := &fakeEmitter{}
	d := itDeps(t, pool, spec, maps, em, nil)
	itJob(t, ctx, pool, itWS, "jcs", spec.Spec.Query)
	// The handler read "pending"; the user cancels before MarkRunning.
	if err := tenancy.InWorkspace(ctx, pool, itWS, func(tx pgx.Tx) error {
		_, err := collectledger.Cancel(ctx, tx, "jcs", time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err := d.For(itWS).Process(ctx, Job{ID: "jcs", Query: spec.Spec.Query, Intent: "market_search", WorkspaceID: itWS,
		Ledger: PgLedger{pool, itWS}, Store: PgStore{pool, itWS}, Emit: em})
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("Process on a job cancelled before start = %v, want ErrCancelled", err)
	}
	j, stages := readJob(t, ctx, pool, itWS, "jcs")
	if j.Status != "cancelled" || j.Attempts != 0 || len(stages) != 0 || len(em.events) != 0 {
		t.Fatalf("cancel overwritten: %+v stages %v events %v", j, stages, em.events)
	}
	if n := countLeads(t, ctx, pool, itWS, "true"); n != 0 {
		t.Fatalf("%d leads stored for a cancelled job", n)
	}
}

// M2: the guard itself: pending -> running only.
func TestMarkRunningGuard(t *testing.T) {
	pool, ctx := itSetup(t)
	l := PgLedger{pool, itWS}
	itJob(t, ctx, pool, itWS, "jm", "q")
	if err := l.MarkRunning(ctx, "jm"); err != nil {
		t.Fatalf("pending -> running: %v", err)
	}
	for _, st := range []string{"running", "done", "failed", "cancelled"} {
		setLedger(t, ctx, pool, itWS, "jm", st, "", 1)
		if err := l.MarkRunning(ctx, "jm"); !errors.Is(err, ErrNotPending) {
			t.Errorf("MarkRunning on %s = %v, want ErrNotPending", st, err)
		}
		if j, _ := readJob(t, ctx, pool, itWS, "jm"); j.Status != st || j.Attempts != 1 {
			t.Errorf("%s row changed: %+v", st, j)
		}
	}
	if err := l.MarkRunning(ctx, "missing"); !errors.Is(err, ErrNotPending) {
		t.Errorf("MarkRunning on a missing row = %v", err)
	}
}
