//go:build integration

package workbook_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/spend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

const wsB = "ws-t10-b"

// RunRows is the loop of the queued run without the lease: only the asked
// rows and columns run, spend settles once, the workbook ends complete.
func TestRunRowsRunsOnlyAskedRowAndColumn(t *testing.T) {
	e := setup(t, wsA, wsB)
	ids := e.seed(wsA, "wb-rr", paidCol, 3, 10)
	fr := paidRunner(0)
	d := e.deps(newSim(t, nil), nil)
	d.Runner = fr
	sum, err := workbook.RunRows(e.ctx, d, wsA, "wb-rr", []string{"c1"}, []int32{ids[1]}, 10*time.Second)
	if err != nil || sum.Completed != 1 || sum.Errors != 0 || sum.Error != "" {
		t.Fatalf("%+v %v", sum, err)
	}
	if n := fr.callCount(); n != 1 {
		t.Fatalf("provider called %d times", n)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_rows WHERE workbook_id='wb-rr' AND enrichments::jsonb->'c1'->>'status'='complete'`); n != 1 {
		t.Fatalf("%d rows enriched, want 1", n)
	}
	if got := e.spentUSD(wsA, "wb-rr"); got != 0.04 {
		t.Fatalf("spent %v", got)
	}
	if st := e.str(wsA, `SELECT status FROM workbooks WHERE id='wb-rr'`); st != "complete" {
		t.Fatalf("workbook status %q", st)
	}
	// A column that is not asked for never runs.
	sum, err = workbook.RunRows(e.ctx, d, wsA, "wb-rr", []string{"nope"}, []int32{ids[2]}, 10*time.Second)
	if err != nil || sum.Completed != 0 || fr.callCount() != 1 {
		t.Fatalf("%+v %v calls=%d", sum, err, fr.callCount())
	}
}

// Another workspace's workbook is invisible: not found, nothing runs or is paid.
func TestRunRowsTenantIsolation(t *testing.T) {
	e := setup(t, wsA, wsB)
	ids := e.seed(wsA, "wb-iso", paidCol, 1, 10)
	fr := paidRunner(0)
	d := e.deps(newSim(t, nil), nil)
	d.Runner = fr
	sum, err := workbook.RunRows(e.ctx, d, wsB, "wb-iso", []string{"c1"}, []int32{ids[0]}, 10*time.Second)
	if err != nil || sum.Error != "workbook_not_found" || fr.callCount() != 0 {
		t.Fatalf("%+v %v calls=%d", sum, err, fr.callCount())
	}
	if got := e.spentUSD(wsA, "wb-iso"); got != 0 {
		t.Fatalf("spent %v", got)
	}
}

// RunRows cannot run without a provider runner: an error, not a nil dereference.
func TestRunRowsNeedsRunner(t *testing.T) {
	e := setup(t, wsA)
	d := e.deps(newSim(t, nil), nil)
	if _, err := workbook.RunRows(e.ctx, d, wsA, "wb-x", []string{"c1"}, []int32{1}, time.Second); err == nil {
		t.Fatal("expected error")
	}
}

// Cancelling the caller after the paid dispatch leaves the attempt uncertain
// (never settled, never released): the action reports a failure, not a success.
func TestRunRowsCancelMidCallLeavesSpendUncertain(t *testing.T) {
	e := setup(t, wsA)
	ids := e.seed(wsA, "wb-rr-cancel", paidCol, 1, 10)
	started := make(chan struct{})
	fr := &fakeRunner{fn: func(ctx context.Context, _ string, _ providers.Input) (providers.Output, error) {
		close(started)
		<-ctx.Done()
		return providers.Output{}, ctx.Err()
	}}
	d := e.deps(newSim(t, nil), nil)
	d.Runner = fr
	ctx, cancel := context.WithCancel(e.ctx)
	go func() { <-started; cancel() }()
	_, err := workbook.RunRows(ctx, d, wsA, "wb-rr-cancel", []string{"c1"}, []int32{ids[0]}, 10*time.Second)
	if err == nil {
		t.Fatal("expected an error after cancel")
	}
	if st := e.str(wsA, `SELECT status FROM workbook_spend_attempts`); st != "uncertain" {
		t.Fatalf("attempt status %q, want uncertain", st)
	}
}

// A live queued run of the same workbook and a one-row run share the spend
// attempt key: the provider is called and paid once.
func TestRunRowsConcurrentWithLiveQueuedRunSpendsOnce(t *testing.T) {
	e := setup(t, wsA)
	ids := e.seed(wsA, "wb-rr-race", paidCol, 1, 10)
	fr := paidRunner(150 * time.Millisecond)
	d := e.deps(newSim(t, nil), nil)
	d.Runner = fr
	c := e.claim(wsA, payloadNoManifests(wsA, "wb-rr-race", map[string]float64{"hunter_io": 0.04}, nil))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = workbook.RunRows(e.ctx, d, wsA, "wb-rr-race", []string{"c1"}, []int32{ids[0]}, 10*time.Second)
	}()
	go func() { defer wg.Done(); _, _ = workbook.Run(e.ctx, d, c) }()
	wg.Wait()
	if n := fr.callCount(); n != 1 {
		t.Fatalf("provider called %d times, want 1", n)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_spend_attempts`); n != 1 {
		t.Fatalf("%d spend attempts, want 1", n)
	}
	if got := e.spentUSD(wsA, "wb-rr-race"); got != 0.04 {
		t.Fatalf("spent %v, want 0.04 once", got)
	}
	want := spend.AttemptKey(wsA, "wb-rr-race", c.ID, fmt.Sprintf("row:%d", ids[0]), "c1", "hunter_io")
	if got := e.str(wsA, `SELECT attempt_key FROM workbook_spend_attempts`); got != want {
		t.Fatalf("attempt key %q is not the queued run's key %q", got, want)
	}
}
