//go:build integration

package workbook_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

const wsA = "ws-t12-a"

func keys(m map[string]any) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

func TestRunHappyPath(t *testing.T) {
	e := setup(t, wsA)
	if e.redis == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	ids := e.seed(wsA, "wb-happy", "["+colJSON("c1", "p1")+"]", 3, 10)
	col, pub := e.subscribe("wb-happy")
	s := newSim(t, nil)
	c := e.claim(wsA, payloadFor(wsA, "wb-happy", map[string]float64{"p1": 0.05}, nil))

	res, err := workbook.Run(e.ctx, e.deps(s, pub), c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed != 3 || res.Errors != 0 || res.Total != 3 || res.Rows != 3 || res.Stopped {
		t.Fatalf("result %+v", res)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_enrichments WHERE workbook_id='wb-happy' AND status='complete' AND value='1999' AND provider='p1'`); n != 3 {
		t.Fatalf("complete cells %d", n)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_spend_attempts WHERE workbook_id='wb-happy' AND status='settled' AND settled_microusd=50000`); n != 3 {
		t.Fatalf("settled attempts %d", n)
	}
	if got := e.spentUSD(wsA, "wb-happy"); math.Abs(got-0.15) > 1e-9 {
		t.Fatalf("budget_spent_usd %v", got)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_rows WHERE workbook_id='wb-happy' AND enrichments::jsonb->'c1'->>'status'='complete' AND enrichments::jsonb->'c1'->>'value'='1999' AND enrichments::jsonb->'c1'->>'provider'='p1'`); n != 3 {
		t.Fatalf("overlay entries %d", n)
	}
	if st := e.str(wsA, `SELECT status FROM workbooks WHERE id='wb-happy'`); st != "complete" {
		t.Fatalf("workbook status %s", st)
	}
	if n := e.one(wsA, `SELECT completed_rows FROM workbooks WHERE id='wb-happy'`); n != 3 {
		t.Fatalf("completed_rows %d", n)
	}

	msgs := col.wait(t, 7)
	var running, results, status int
	for _, m := range msgs {
		switch m["type"] {
		case "cell_update":
			if m["status"] == "running" {
				running++
				if keys(m) != "colId,leadId,rowId,status,type,value" {
					t.Fatalf("running keys %s", keys(m))
				}
			} else {
				results++
				if keys(m) != "colId,error,leadId,provenance,provider,research,rowId,status,type,value,verify_status" {
					t.Fatalf("result keys %s", keys(m))
				}
				if m["status"] != "complete" || m["value"] != "1999" || m["provider"] != "p1" || m["error"] != nil {
					t.Fatalf("result msg %v", m)
				}
			}
		case "workbook_status":
			status++
			if keys(m) != "completed,errors,status,total,type" || m["status"] != "complete" || m["completed"] != 3.0 || m["errors"] != 0.0 || m["total"] != 3.0 {
				t.Fatalf("status msg %v", m)
			}
		}
	}
	if running != 3 || results != 3 || status != 1 {
		t.Fatalf("running=%d results=%d status=%d", running, results, status)
	}
	_ = ids
}

func TestWaterfallStopsAtFirstSuccess(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-wf", "["+colJSON("c1", "free1", "p2", "p3")+"]", 1, 10)
	s := newSim(t, func(name string, _ int, w http.ResponseWriter, _ *http.Request) {
		if name == "free1" {
			fmt.Fprint(w, `{"v":null}`) // no match
			return
		}
		fmt.Fprint(w, `{"v":"2001"}`)
	})
	c := e.claim(wsA, payloadFor(wsA, "wb-wf", map[string]float64{"free1": 0, "p2": 0.05, "p3": 0.05}, nil))
	res, err := workbook.Run(e.ctx, e.deps(s, nil), c)
	if err != nil || res.Completed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if s.count("free1") != 1 || s.count("p2") != 1 || s.count("p3") != 0 {
		t.Fatalf("calls free1=%d p2=%d p3=%d", s.count("free1"), s.count("p2"), s.count("p3"))
	}
	if v := e.str(wsA, `SELECT value FROM workbook_enrichments WHERE workbook_id='wb-wf'`); v != "2001" {
		t.Fatalf("value %q", v)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_spend_attempts WHERE workbook_id='wb-wf'`); n != 1 {
		t.Fatalf("attempts %d (free providers are never reserved)", n)
	}
	// Telemetry: both providers recorded, the free miss as a non-hit.
	if h := e.one(wsA, `SELECT hits FROM provider_stats WHERE provider='p2' AND field='founded_year'`); h != 1 {
		t.Fatalf("p2 hits %d", h)
	}
	if h := e.one(wsA, `SELECT attempts*10+hits FROM provider_stats WHERE provider='free1' AND field='founded_year'`); h != 10 {
		t.Fatalf("free1 attempts/hits %d", h)
	}
}

func TestNoMatchIsError_no_data(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-nd", "["+colJSON("c1", "free1")+"]", 2, 0)
	s := newSim(t, func(_ string, _ int, w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"v":""}`) })
	c := e.claim(wsA, payloadFor(wsA, "wb-nd", map[string]float64{"free1": 0}, nil))
	res, err := workbook.Run(e.ctx, e.deps(s, nil), c)
	if err != nil || res.Completed != 0 || res.Errors != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_enrichments WHERE status='error' AND error='no_data' AND value IS NULL`); n != 2 {
		t.Fatalf("error cells %d", n)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_rows WHERE enrichments::jsonb->'c1'->>'error'='no_data' AND enrichments::jsonb->'c1'->>'status'='error'`); n != 2 {
		t.Fatalf("overlay errors %d", n)
	}
	if st := e.str(wsA, `SELECT status FROM workbooks WHERE id='wb-nd'`); st != "failed" {
		t.Fatalf("workbook status %s", st)
	}
}

func TestBudgetExhaustionStopsPaidCalls(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-bud", "["+colJSON("c1", "p1")+"]", 3, 0.12)
	s := newSim(t, nil)
	c := e.claim(wsA, payloadFor(wsA, "wb-bud", map[string]float64{"p1": 0.05}, nil))
	res, err := workbook.Run(e.ctx, e.deps(s, nil), c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed != 2 || res.Errors != 1 || s.count("p1") != 2 {
		t.Fatalf("%+v calls=%d", res, s.count("p1"))
	}
	if msg := e.str(wsA, `SELECT error FROM workbook_enrichments WHERE status='error'`); msg != "providers_unavailable: p1 (over_budget)" {
		t.Fatalf("error %q", msg)
	}
	if got := e.spentUSD(wsA, "wb-bud"); math.Abs(got-0.10) > 1e-9 {
		t.Fatalf("spent %v", got)
	}
}

func TestFillMissingSkipsComplete(t *testing.T) {
	e := setup(t, wsA)
	ids := e.seed(wsA, "wb-fm", "["+colJSON("c1", "free1")+"]", 3, 0)
	e.inWS(wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `UPDATE workbook_rows SET enrichments='{"c1":{"value":"keep","status":"complete","provider":"x","error":null}}'::json WHERE id=$1`, ids[0])
		return err
	})
	s := newSim(t, nil)
	c := e.claim(wsA, payloadFor(wsA, "wb-fm", map[string]float64{"free1": 0}, map[string]any{"fill_missing": true}))
	res, err := workbook.Run(e.ctx, e.deps(s, nil), c)
	if err != nil || res.Total != 2 || res.Completed != 2 || s.count("free1") != 2 {
		t.Fatalf("%+v calls=%d err=%v", res, s.count("free1"), err)
	}
	if v := e.str(wsA, `SELECT enrichments::jsonb->'c1'->>'value' FROM workbook_rows WHERE id=$1`, ids[0]); v != "keep" {
		t.Fatalf("complete cell clobbered: %q", v)
	}
	if res.Rows != 3 {
		t.Fatalf("rows %d", res.Rows)
	}
}

func TestPauseStopsPromptly(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-pause", "["+colJSON("c1", "free1")+"]", 20, 0)
	s := newSim(t, func(_ string, n int, w http.ResponseWriter, _ *http.Request) {
		time.Sleep(40 * time.Millisecond)
		fmt.Fprint(w, `{"v":"1"}`)
	})
	c := e.claim(wsA, payloadFor(wsA, "wb-pause", map[string]float64{"free1": 0}, nil))
	go func() {
		for s.total() < 3 {
			time.Sleep(5 * time.Millisecond)
		}
		e.inWS(wsA, func(tx pgx.Tx) error {
			_, err := tx.Exec(e.ctx, `UPDATE workbooks SET status='paused' WHERE id='wb-pause'`)
			return err
		})
	}()
	col, pub := e.subscribe("wb-pause")
	res, err := workbook.Run(e.ctx, e.deps(s, pub), c)
	if err != nil {
		t.Fatal(err)
	}
	if s.total() >= 20 || s.total() < 3 {
		t.Fatalf("attempted %d cells", s.total())
	}
	if st := e.str(wsA, `SELECT status FROM workbooks WHERE id='wb-pause'`); st != "paused" {
		t.Fatalf("status %s", st)
	}
	if e.redis != "" {
		msgs := col.wait(t, 1)
		last := msgs[len(msgs)-1]
		for i := 0; i < 100 && last["type"] != "workbook_status"; i++ {
			time.Sleep(10 * time.Millisecond)
			msgs = col.wait(t, 1)
			last = msgs[len(msgs)-1]
		}
		if last["type"] != "workbook_status" || last["status"] != "paused" {
			t.Fatalf("last message %v", last)
		}
	}
	_ = res
}

func TestCancelMidWaterfall(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-cancel", "["+colJSON("c1", "free1", "p2", "p3")+"]", 1, 10)
	held := make(chan struct{})
	s := newSim(t, func(name string, _ int, w http.ResponseWriter, r *http.Request) {
		switch name {
		case "free1":
			fmt.Fprint(w, `{"v":null}`)
		case "p2":
			close(held)
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		default:
			fmt.Fprint(w, `{"v":"3"}`)
		}
	})
	c := e.claim(wsA, payloadFor(wsA, "wb-cancel", map[string]float64{"free1": 0, "p2": 0.05, "p3": 0.05}, nil))
	ctx, stop := e.watch(c)
	defer stop()
	go func() {
		<-held
		if _, err := e.pool.Exec(e.ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, c.ID); err != nil {
			t.Error(err)
		}
	}()
	_, err := workbook.Run(ctx, e.deps(s, nil), c)
	if err == nil {
		t.Fatal("expected the run to unwind with an error")
	}
	if s.count("p3") != 0 {
		t.Fatal("a third provider call started after cancellation")
	}
	if st := e.str(wsA, `SELECT status FROM workbook_spend_attempts WHERE provider='p2'`); st != "uncertain" {
		t.Fatalf("in-flight attempt is %q, want uncertain", st)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_spend_attempts WHERE provider='p3'`); n != 0 {
		t.Fatal("p3 was reserved")
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_enrichments`); n != 0 {
		t.Fatalf("cell written after cancel: %d", n)
	}
	var jobStatus string
	if err := e.pool.QueryRow(e.ctx, `SELECT status FROM jobs WHERE id=$1`, c.ID).Scan(&jobStatus); err != nil || jobStatus != "cancelled" {
		t.Fatalf("job status %q %v", jobStatus, err)
	}
}

func TestLeaseLossAbortsCommits(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-lease", "["+colJSON("c1", "p1")+"]", 3, 10)
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s := newSim(t, func(_ string, _ int, w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(held); <-release })
		fmt.Fprint(w, `{"v":"1"}`)
	})
	c := e.claim(wsA, payloadFor(wsA, "wb-lease", map[string]float64{"p1": 0.05}, nil))
	go func() {
		<-held
		// Another executor reclaimed the job: the lease token moves on.
		if _, err := e.pool.Exec(e.ctx, `UPDATE jobs SET lease_token = lease_token + 1 WHERE id=$1`, c.ID); err != nil {
			t.Error(err)
		}
		close(release)
	}()
	_, err := workbook.Run(e.ctx, e.deps(s, nil), c)
	if !errors.Is(err, workbook.ErrLeaseLost) {
		t.Fatalf("want ErrLeaseLost, got %v", err)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_enrichments`); n != 0 {
		t.Fatalf("cell committed after lease loss: %d", n)
	}
	if s.total() != 1 {
		t.Fatalf("calls after lease loss: %d", s.total())
	}
	if st := e.str(wsA, `SELECT status FROM workbooks WHERE id='wb-lease'`); st != "running" {
		t.Fatalf("workbook state touched by a non-owner: %s", st)
	}
}

// The "crash" is a context kill: nothing is finalized, the reaper's CAS UPDATE
// requeues the row, and a new worker reclaims it with the next lease token.
func TestCrashThenPythonReapsThenGoResumes(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-crash", "["+colJSON("c1", "p1")+"]", 4, 10)
	held := make(chan struct{})
	s := newSim(t, func(_ string, n int, w http.ResponseWriter, r *http.Request) {
		if n == 3 {
			close(held)
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		fmt.Fprint(w, `{"v":"7"}`)
	})
	pl := payloadFor(wsA, "wb-crash", map[string]float64{"p1": 0.05}, nil)
	c1 := e.claim(wsA, pl)
	ctx, kill := context.WithCancel(e.ctx)
	go func() { <-held; kill() }()
	if _, err := workbook.Run(ctx, e.deps(s, nil), c1); err == nil {
		t.Fatal("killed run should not succeed")
	}
	if got := e.spentUSD(wsA, "wb-crash"); math.Abs(got-0.10) > 1e-9 {
		t.Fatalf("spent before reap %v", got)
	}

	// Python's reap_dead_jobs_once CAS (_transition_dead_jobs, will_retry branch).
	tag, err := e.pool.Exec(e.ctx, `UPDATE jobs SET status='pending', worker_id=NULL, locked_at=NULL, retry_count=retry_count+1,
	  started_at=NULL, next_run_at=NULL, error='Retry 1: Heartbeat Timeout'
	  WHERE id=$1 AND status='processing' AND worker_id=$2 AND lease_token=$3 AND locked_at=$4`,
		c1.ID, c1.WorkerID, c1.LeaseToken, c1.LockedAt)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("reap CAS: %v %v", tag, err)
	}
	c2 := e.reclaim()
	if c2.ID != c1.ID || c2.LeaseToken != c1.LeaseToken+1 {
		t.Fatalf("reclaim %+v vs %+v", c2, c1)
	}
	if _, err := workbook.Run(e.ctx, e.deps(s, nil), c2); err != nil {
		t.Fatal(err)
	}
	if s.total() != 4 {
		t.Fatalf("provider calls %d: settled attempts must be replayed, not re-dispatched", s.total())
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_spend_attempts WHERE status='settled'`); n != 3 {
		t.Fatalf("settled %d", n)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_spend_attempts WHERE status='uncertain'`); n != 1 {
		t.Fatalf("uncertain %d", n)
	}
	if got := e.spentUSD(wsA, "wb-crash"); math.Abs(got-0.15) > 1e-9 {
		t.Fatalf("double charge: spent %v", got)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_enrichments WHERE status='complete' AND value='7'`); n != 3 {
		t.Fatalf("complete cells %d", n)
	}
	if msg := e.str(wsA, `SELECT error FROM workbook_enrichments WHERE status='error'`); msg != "attempt_not_dispatchable" {
		t.Fatalf("uncertain attempt must stay blocked, got %q", msg)
	}
}

func TestTenantIsolationTwoWorkspacesConcurrent(t *testing.T) {
	const wsB = "ws-t12-b"
	e := setup(t, wsA, wsB)
	e.seed(wsA, "wb-iso-a", "["+colJSON("c1", "p1")+"]", 4, 10)
	e.seed(wsB, "wb-iso-b", "["+colJSON("c1", "p1")+"]", 4, 10)
	s := newSim(t, nil)
	ca := e.claim(wsA, payloadFor(wsA, "wb-iso-a", map[string]float64{"p1": 0.05}, map[string]any{"concurrency": 2}))
	cb := e.claim(wsB, payloadFor(wsB, "wb-iso-b", map[string]float64{"p1": 0.05}, map[string]any{"concurrency": 2}))
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); _, errs[0] = workbook.Run(e.ctx, e.deps(s, nil), ca) }()
	go func() { defer wg.Done(); _, errs[1] = workbook.Run(e.ctx, e.deps(s, nil), cb) }()
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("%v %v", errs[0], errs[1])
	}
	for _, p := range []struct{ ws, wb, other string }{{wsA, "wb-iso-a", "wb-iso-b"}, {wsB, "wb-iso-b", "wb-iso-a"}} {
		if n := e.one(p.ws, `SELECT count(*) FROM workbook_enrichments WHERE workbook_id=$1 AND status='complete'`, p.wb); n != 4 {
			t.Fatalf("%s cells %d", p.ws, n)
		}
		if n := e.one(p.ws, `SELECT count(*) FROM workbook_enrichments WHERE workbook_id=$1`, p.other); n != 0 {
			t.Fatalf("%s sees the other tenant's cells: %d", p.ws, n)
		}
		if n := e.one(p.ws, `SELECT count(*) FROM workbook_spend_attempts WHERE workspace_id <> $1`, p.ws); n != 0 {
			t.Fatalf("%s sees foreign spend attempts", p.ws)
		}
		if got := e.spentUSD(p.ws, p.wb); math.Abs(got-0.20) > 1e-9 {
			t.Fatalf("%s spent %v", p.ws, got)
		}
	}
}

// C1: columns whose ids equal lead fields (email, phone) must not leak their own
// outputs into later spend contracts: a resumed run keeps the finished cell,
// creates no new settled attempt and never writes contract_conflict.
func TestResumeKeepsFinishedCellWhenColumnIDsEqualLeadFields(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-c1", "["+colJSON("email", "p1")+","+colJSON("phone", "p2")+"]", 1, 10)
	held := make(chan struct{})
	s := newSim(t, func(name string, _ int, w http.ResponseWriter, r *http.Request) {
		if name == "p2" {
			close(held)
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		fmt.Fprint(w, `{"v":"7"}`)
	})
	pl := payloadFor(wsA, "wb-c1", map[string]float64{"p1": 0.05, "p2": 0.05}, nil)
	c1 := e.claim(wsA, pl)
	ctx, kill := context.WithCancel(e.ctx)
	go func() { <-held; kill() }()
	if _, err := workbook.Run(ctx, e.deps(s, nil), c1); err == nil {
		t.Fatal("killed run should not succeed")
	}
	if tag, err := e.pool.Exec(e.ctx, `UPDATE jobs SET status='pending', worker_id=NULL, locked_at=NULL, retry_count=retry_count+1,
	  started_at=NULL, next_run_at=NULL WHERE id=$1 AND status='processing' AND lease_token=$2`, c1.ID, c1.LeaseToken); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("reap: %v %v", tag, err)
	}
	c2 := e.reclaim()
	if _, err := workbook.Run(e.ctx, e.deps(s, nil), c2); err != nil {
		t.Fatal(err)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_enrichments WHERE column_id='email' AND status='complete' AND value='7'`); n != 1 {
		t.Fatalf("finished email cell was overwritten (complete cells: %d)", n)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_spend_attempts WHERE column_id='email'`); n != 1 {
		t.Fatalf("email attempts %d: resume must replay the settled attempt, not create another", n)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_spend_attempts WHERE status='settled'`); n != 1 {
		t.Fatalf("settled attempts %d", n)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_enrichments WHERE error='contract_conflict'`); n != 0 {
		t.Fatalf("contract_conflict written %d times", n)
	}
	if s.count("p1") != 1 {
		t.Fatalf("p1 called %d times", s.count("p1"))
	}
}

// A real input change on resume yields contract_conflict from the ledger; the
// finished paid cell is still kept as is (never overwritten by the conflict).
func TestContractConflictNeverOverwritesCompleteCell(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-c1b", "["+colJSON("c1", "p1")+"]", 1, 10)
	s := newSim(t, func(_ string, _ int, w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"v":"7"}`) })
	pl := payloadFor(wsA, "wb-c1b", map[string]float64{"p1": 0.05}, nil)
	c1 := e.claim(wsA, pl)
	if _, err := workbook.Run(e.ctx, e.deps(s, nil), c1); err != nil {
		t.Fatal(err)
	}
	if tag, err := e.pool.Exec(e.ctx, `UPDATE jobs SET status='pending', worker_id=NULL, locked_at=NULL, completed_at=NULL,
	  started_at=NULL, next_run_at=NULL WHERE id=$1`, c1.ID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("requeue: %v %v", tag, err)
	}
	e.inWS(wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `UPDATE workbook_rows SET data='{"company":"Changed Co","website":"x.example.com"}'::json`)
		return err
	})
	c2 := e.reclaim()
	if _, err := workbook.Run(e.ctx, e.deps(s, nil), c2); err != nil {
		t.Fatal(err)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_enrichments WHERE status='complete' AND value='7'`); n != 1 {
		t.Fatalf("complete cell was overwritten: %d", n)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_enrichments WHERE error='contract_conflict'`); n != 0 {
		t.Fatal("contract_conflict written over a complete cell")
	}
	if s.total() != 1 {
		t.Fatalf("provider calls %d", s.total())
	}
}

func refCol(id, ref string, chain ...string) string {
	c := colJSON(id, chain...)
	return c[:len(c)-1] + `,"prompt":"{` + ref + `}"}`
}

// Providers must be called for upstream columns first even when the dependent
// is configured first.
func TestRunOrderFollowsDependencies(t *testing.T) {
	e := setup(t, wsA)
	var mu sync.Mutex
	var order []string
	s := newSim(t, func(name string, _ int, w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
		fmt.Fprint(w, `{"v":"1999"}`)
	})
	e.seed(wsA, "wb-order", "["+refCol("a", "b", "pa")+","+colJSON("b", "pb")+"]", 1, 10)
	c := e.claim(wsA, payloadFor(wsA, "wb-order", map[string]float64{"pa": 0, "pb": 0}, nil))
	res, err := workbook.Run(e.ctx, e.deps(s, nil), c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed != 2 || res.Errors != 0 {
		t.Fatalf("result %+v", res)
	}
	if strings.Join(order, ",") != "pb,pa" {
		t.Fatalf("provider order %v", order)
	}
}

// a is blocked by the cycle; b then sees its upstream failed (same order as _run_one_row).
func TestCyclicColumnBlockedMessage(t *testing.T) {
	e := setup(t, wsA)
	s := newSim(t, nil)
	e.seed(wsA, "wb-cyc", "["+refCol("a", "b", "pa")+","+refCol("b", "a", "pb")+"]", 1, 10)
	c := e.claim(wsA, payloadFor(wsA, "wb-cyc", map[string]float64{"pa": 0, "pb": 0}, nil))
	res, err := workbook.Run(e.ctx, e.deps(s, nil), c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors != 2 || res.Completed != 0 || s.total() != 0 {
		t.Fatalf("result %+v calls %d", res, s.total())
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_rows WHERE workbook_id='wb-cyc' AND enrichments::jsonb->'a'->>'error'='dependency_cycle' AND enrichments::jsonb->'b'->>'error'='upstream_dependency_failed'`); n != 1 {
		t.Fatalf("cycle error cells %d: %s", n, e.str(wsA, `SELECT enrichments::text FROM workbook_rows WHERE workbook_id='wb-cyc'`))
	}
}

func TestUpstreamFailureSkipsDependent(t *testing.T) {
	e := setup(t, wsA)
	s := newSim(t, func(name string, _ int, w http.ResponseWriter, _ *http.Request) {
		if name == "pb" {
			fmt.Fprint(w, `{}`) // no data: upstream cell fails
			return
		}
		fmt.Fprint(w, `{"v":"1999"}`)
	})
	e.seed(wsA, "wb-up", "["+colJSON("b", "pb")+","+refCol("a", "b", "pa")+"]", 1, 10)
	c := e.claim(wsA, payloadFor(wsA, "wb-up", map[string]float64{"pa": 0, "pb": 0}, nil))
	res, err := workbook.Run(e.ctx, e.deps(s, nil), c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors != 2 || res.Completed != 0 || s.count("pa") != 0 {
		t.Fatalf("result %+v pa calls %d", res, s.count("pa"))
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_rows WHERE workbook_id='wb-up' AND enrichments::jsonb->'b'->>'error'='no_data' AND enrichments::jsonb->'a'->>'error'='upstream_dependency_failed' AND enrichments::jsonb->'a'->>'status'='error'`); n != 1 {
		t.Fatalf("expected upstream_dependency_failed cell")
	}
}

// An upstream executable column that is not part of this run and has no value
// leaves the dependent with Python's upstream_dependency_unavailable.
func TestUpstreamUnavailableBlocksDependent(t *testing.T) {
	e := setup(t, wsA)
	s := newSim(t, nil)
	e.seed(wsA, "wb-unav", "["+colJSON("b", "pb")+","+refCol("a", "b", "pa")+"]", 1, 10)
	c := e.claim(wsA, payloadFor(wsA, "wb-unav", map[string]float64{"pa": 0, "pb": 0}, map[string]any{"column_ids": []string{"a"}}))
	res, err := workbook.Run(e.ctx, e.deps(s, nil), c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors != 1 || s.total() != 0 {
		t.Fatalf("result %+v calls %d", res, s.total())
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_rows WHERE workbook_id='wb-unav' AND enrichments::jsonb->'a'->>'error'='upstream_dependency_unavailable'`); n != 1 {
		t.Fatal("expected upstream_dependency_unavailable")
	}
}

// Until Task 21 non-go column types are still handed back to Python.
func TestRunStillHandsBackNonGoTypes(t *testing.T) {
	e := setup(t, wsA)
	s := newSim(t, nil)
	e.seed(wsA, "wb-nongo", `[{"id":"c1","type":"ai_formula","name":"c1","prompt":"x"}]`, 1, 10)
	c := e.claim(wsA, payloadFor(wsA, "wb-nongo", map[string]float64{"pa": 0}, nil))
	_, err := workbook.Run(e.ctx, e.deps(s, nil), c)
	if !errors.Is(err, workbook.ErrHandedBack) || s.total() != 0 {
		t.Fatalf("non-go type must be handed back: %v", err)
	}
}

// recPub records every published message.
type recPub struct {
	mu   sync.Mutex
	msgs []map[string]any
}

func (r *recPub) Publish(_ context.Context, _ string, msg map[string]any) {
	r.mu.Lock()
	r.msgs = append(r.msgs, msg)
	r.mu.Unlock()
}

// Python returns on a dependency error before the "running" broadcast.
func TestCycleCellEmitsNoRunningEvent(t *testing.T) {
	e := setup(t, wsA)
	s := newSim(t, nil)
	e.seed(wsA, "wb-cycev", "["+refCol("a", "b", "pa")+","+refCol("b", "a", "pb")+"]", 1, 10)
	c := e.claim(wsA, payloadFor(wsA, "wb-cycev", map[string]float64{"pa": 0, "pb": 0}, nil))
	pub := &recPub{}
	if _, err := workbook.Run(e.ctx, e.deps(s, pub), c); err != nil {
		t.Fatal(err)
	}
	for _, m := range pub.msgs {
		if m["type"] == "cell_update" {
			t.Fatalf("blocked cell emitted a cell_update: %v", m)
		}
	}
}

// One row's upstream failure must not affect another row.
func TestUpstreamFailureIsolatedPerRow(t *testing.T) {
	e := setup(t, wsA)
	s := newSim(t, func(name string, n int, w http.ResponseWriter, _ *http.Request) {
		if name == "pb" && n == 1 {
			fmt.Fprint(w, `{}`) // first row's upstream finds nothing
			return
		}
		fmt.Fprint(w, `{"v":"1999"}`)
	})
	ids := e.seed(wsA, "wb-iso", "["+colJSON("b", "pb")+","+refCol("a", "b", "pa")+"]", 2, 10)
	c := e.claim(wsA, payloadFor(wsA, "wb-iso", map[string]float64{"pa": 0, "pb": 0}, nil))
	res, err := workbook.Run(e.ctx, e.deps(s, nil), c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed != 2 || res.Errors != 2 {
		t.Fatalf("result %+v", res)
	}
	q := func(id int32) string {
		return e.str(wsA, fmt.Sprintf(`SELECT coalesce(enrichments::jsonb->'a'->>'error', enrichments::jsonb->'a'->>'status') FROM workbook_rows WHERE id=%d`, id))
	}
	if q(ids[0]) != "upstream_dependency_failed" || q(ids[1]) != "complete" {
		t.Fatalf("rows not isolated: %q %q", q(ids[0]), q(ids[1]))
	}
}
