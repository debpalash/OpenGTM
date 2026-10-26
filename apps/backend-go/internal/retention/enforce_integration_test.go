//go:build integration

package retention

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

var minimumDays = map[string]int{"audit": 90, "llm_usage": 30, "signals": 30, "activation": 30, "audience_history": 30, "agent_results": 30, "outreach_history": 30}

var vecNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

func (e *env) deps() Deps {
	return Deps{Pool: e.pool, Now: func() time.Time { return vecNow }}
}

func (e *env) putPolicy(t *testing.T, ws string, enabled, hold bool, days map[string]int) {
	t.Helper()
	b, _ := json.Marshal(days)
	if _, err := e.owner.Exec(context.Background(),
		`INSERT INTO retention_policies (workspace_id,enabled,legal_hold,retention_days) VALUES ($1,$2,$3,$4)
		 ON CONFLICT (workspace_id) DO UPDATE SET enabled=$2, legal_hold=$3, retention_days=$4`, ws, enabled, hold, b); err != nil {
		t.Fatal(err)
	}
}

func (e *env) putRun(t *testing.T, ws, status string, by any, snap map[string]int) string {
	t.Helper()
	b, _ := json.Marshal(snap)
	var id string
	if err := e.owner.QueryRow(context.Background(),
		`INSERT INTO retention_runs (id,workspace_id,status,requested_by,policy_snapshot,deleted_counts)
		 VALUES (gen_random_uuid()::text,$1,$2,$3,$4,'{}') RETURNING id`, ws, status, by, b).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

type runRow struct {
	Status, RequestedBy, Error string
	Counts                     map[string]int64
	Started, Finished          bool
	Snapshot                   map[string]int
}

func (e *env) getRun(t *testing.T, id string) runRow {
	t.Helper()
	var r runRow
	var by, er *string
	var counts, snap []byte
	if err := e.owner.QueryRow(context.Background(),
		`SELECT status, requested_by, error, deleted_counts::text, policy_snapshot::text, started_at IS NOT NULL, finished_at IS NOT NULL FROM retention_runs WHERE id=$1`, id).
		Scan(&r.Status, &by, &er, &counts, &snap, &r.Started, &r.Finished); err != nil {
		t.Fatal(err)
	}
	if by != nil {
		r.RequestedBy = *by
	}
	if er != nil {
		r.Error = *er
	}
	_ = json.Unmarshal(counts, &r.Counts)
	_ = json.Unmarshal(snap, &r.Snapshot)
	return r
}

func (e *env) runsFor(t *testing.T, ws string) []string {
	t.Helper()
	rows, err := e.owner.Query(context.Background(), `SELECT id FROM retention_runs WHERE workspace_id=$1 ORDER BY created_at, id`, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids
}

func (e *env) cleanRuns() {
	_, _ = e.owner.Exec(context.Background(), `DELETE FROM retention_runs WHERE workspace_id LIKE $1`, e.prefix+"%")
}

func setupEnf(t *testing.T) *env {
	e := setup(t)
	e.cleanRuns()
	t.Cleanup(e.cleanRuns)
	return e
}

func (e *env) total(t *testing.T, table, ws string) int64 {
	t.Helper()
	var n int64
	if err := e.owner.QueryRow(context.Background(), `SELECT count(*) FROM `+table+` WHERE workspace_id=$1`, ws).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *env) totals(t *testing.T, ws string) map[string]int64 {
	m := map[string]int64{}
	for _, tb := range seededTables[:8] {
		m[tb] = e.total(t, tb, ws)
	}
	return m
}

func expectedMinimums(t *testing.T, e *env, ws string) map[string]int64 {
	v := loadVectors(t, nil)
	for _, c := range v.Cases {
		if c.Name == "minimums" {
			return c.Expected["UTC"][strings.TrimPrefix(ws, e.prefix)]
		}
	}
	t.Fatal("no minimums case")
	return nil
}

func TestEnforceDeletesOnlyExpiredPerCategory(t *testing.T) {
	e := setupEnf(t)
	v := loadVectors(t, e.wsMap())
	e.seed(t, v, vecNow)
	a, b := e.prefix+"wsA", e.prefix+"wsB"
	e.putPolicy(t, a, true, false, minimumDays)
	beforeA, beforeB := e.totals(t, a), e.totals(t, b)
	run := e.putRun(t, a, "pending", "manual", minimumDays)
	if err := Enforce(context.Background(), e.deps(), a, run); err != nil {
		t.Fatal(err)
	}
	if got := e.totals(t, b); !equalMaps(got, beforeB) {
		t.Fatalf("workspace B changed: %v -> %v", beforeB, got)
	}
	afterA := e.totals(t, a)
	if equalMaps(afterA, beforeA) {
		t.Fatal("nothing deleted in A")
	}
	if left := e.preview(t, a, "", minimumDays, vecNow); sumCounts(left) != 0 {
		t.Fatalf("expired rows remain: %v", left)
	}
	// Rows newer than the cutoff are all kept: B (same seed, untouched) still previews as the vector expects.
	want := expectedMinimums(t, e, b)
	got := e.preview(t, b, "", minimumDays, vecNow)
	for cat, n := range want {
		if got[cat] != n {
			t.Errorf("B %s: %d want %d", cat, got[cat], n)
		}
	}
	if r := e.getRun(t, run); r.Status != "completed" || !r.Started || !r.Finished || r.Error != "" {
		t.Fatalf("run %+v", r)
	}
}

func TestEnforceWorkspaceScopeWithoutRLS(t *testing.T) {
	e := setupEnf(t)
	v := loadVectors(t, e.wsMap())
	e.seed(t, v, vecNow)
	a, b := e.prefix+"wsA", e.prefix+"wsB"
	e.putPolicy(t, a, true, false, minimumDays)
	beforeB := e.totals(t, b)
	run := e.putRun(t, a, "pending", "manual", minimumDays)
	d := e.deps()
	d.Pool = e.owner // superuser: RLS cannot help, only the explicit workspace predicate
	if err := Enforce(context.Background(), d, a, run); err != nil {
		t.Fatal(err)
	}
	if got := e.totals(t, b); !equalMaps(got, beforeB) {
		t.Fatalf("workspace B changed without RLS: %v -> %v", beforeB, got)
	}
}

func TestEnforceCountsMatchFixture(t *testing.T) {
	e := setupEnf(t)
	v := loadVectors(t, e.wsMap())
	e.seed(t, v, vecNow)
	a := e.prefix + "wsA"
	e.putPolicy(t, a, true, false, minimumDays)
	run := e.putRun(t, a, "pending", "manual", minimumDays)
	if err := Enforce(context.Background(), e.deps(), a, run); err != nil {
		t.Fatal(err)
	}
	r := e.getRun(t, run)
	want := expectedMinimums(t, e, a)
	if len(r.Counts) != 7 {
		t.Fatalf("counts %v", r.Counts)
	}
	for cat, n := range want {
		if r.Counts[cat] != n {
			t.Errorf("%s: deleted %d want %d (Python)", cat, r.Counts[cat], n)
		}
	}
	var raw string
	_ = e.owner.QueryRow(context.Background(), `SELECT deleted_counts::text FROM retention_runs WHERE id=$1`, run).Scan(&raw)
	if strings.Index(raw, `"audit"`) > strings.Index(raw, `"llm_usage"`) || strings.Index(raw, `"activation"`) > strings.Index(raw, `"audience_history"`) {
		t.Errorf("counts key order: %s", raw)
	}
}

func TestEnforceBatchesStopOnLegalHoldMidRun(t *testing.T) {
	e := setupEnf(t)
	v := loadVectors(t, e.wsMap())
	e.seed(t, v, vecNow)
	a := e.prefix + "wsA"
	e.putPolicy(t, a, true, false, minimumDays)
	run := e.putRun(t, a, "pending", "manual", minimumDays)
	d := e.deps()
	d.BatchRows = 2
	var batches int32
	d.AfterBatch = func(cat string, n int64) {
		if atomic.AddInt32(&batches, 1) == 2 {
			e.putPolicy(t, a, true, true, minimumDays)
		}
	}
	if err := Enforce(context.Background(), d, a, run); err != nil {
		t.Fatal(err)
	}
	r := e.getRun(t, run)
	if r.Status != "cancelled" || r.Error != "Legal hold enabled during retention enforcement" || !r.Finished {
		t.Fatalf("run %+v", r)
	}
	if atomic.LoadInt32(&batches) != 2 {
		t.Fatalf("ran %d batches after the hold", batches)
	}
	var sum int64
	for _, n := range r.Counts {
		sum += n
	}
	if sum != 4 || len(r.Counts) == 0 || len(r.Counts) >= 7 {
		t.Fatalf("counts so far %v (want 4 rows over the categories reached)", r.Counts)
	}
	if left := e.preview(t, a, "", minimumDays, vecNow); sumCounts(left) == 0 {
		t.Fatal("hold did not stop deletion")
	}
}

func TestEnforceStopsWhenRunCancelled(t *testing.T) {
	e := setupEnf(t)
	v := loadVectors(t, e.wsMap())
	e.seed(t, v, vecNow)
	a := e.prefix + "wsA"
	e.putPolicy(t, a, true, false, minimumDays)
	run := e.putRun(t, a, "pending", "manual", minimumDays)
	d := e.deps()
	d.BatchRows = 2
	var batches int32
	d.AfterBatch = func(string, int64) {
		if atomic.AddInt32(&batches, 1) == 1 {
			_, _ = e.owner.Exec(context.Background(), `UPDATE retention_runs SET status='cancelled', error='stop' WHERE id=$1`, run)
		}
	}
	if err := Enforce(context.Background(), d, a, run); err != nil {
		t.Fatal(err)
	}
	r := e.getRun(t, run)
	if r.Status != "cancelled" || r.Error != "stop" || batches != 1 {
		t.Fatalf("run %+v batches %d", r, batches)
	}
	if r.Counts["audit"] != 2 {
		t.Fatalf("counts so far %v", r.Counts)
	}
}

func TestEnforceSchedulerRunWhenDisabledIsCancelled(t *testing.T) {
	e := setupEnf(t)
	v := loadVectors(t, e.wsMap())
	e.seed(t, v, vecNow)
	a := e.prefix + "wsA"
	e.putPolicy(t, a, false, false, minimumDays)
	before := e.totals(t, a)
	run := e.putRun(t, a, "pending", "scheduler", minimumDays)
	if err := Enforce(context.Background(), e.deps(), a, run); err != nil {
		t.Fatal(err)
	}
	r := e.getRun(t, run)
	if r.Status != "cancelled" || r.Error != "legal hold or scheduled retention disabled" || !r.Finished || r.Started {
		t.Fatalf("run %+v", r)
	}
	if !equalMaps(e.totals(t, a), before) {
		t.Fatal("rows deleted")
	}
	// A manual run on a disabled policy still executes.
	m := e.putRun(t, a, "pending", "manual", minimumDays)
	if err := Enforce(context.Background(), e.deps(), a, m); err != nil {
		t.Fatal(err)
	}
	if got := e.getRun(t, m).Status; got != "completed" {
		t.Fatalf("manual run %s", got)
	}
	// Legal hold cancels even a manual run.
	e.putPolicy(t, a, true, true, minimumDays)
	h := e.putRun(t, a, "pending", "manual", minimumDays)
	if err := Enforce(context.Background(), e.deps(), a, h); err != nil {
		t.Fatal(err)
	}
	if r := e.getRun(t, h); r.Status != "cancelled" || r.Error != "legal hold or scheduled retention disabled" {
		t.Fatalf("hold run %+v", r)
	}
}

func TestEnforceCreatesRunWhenMissing(t *testing.T) {
	e := setupEnf(t)
	a := e.prefix + "wsA"
	e.putPolicy(t, a, true, false, map[string]int{"audit": 120})
	for _, id := range []string{"", "not-a-real-run"} {
		if err := Enforce(context.Background(), e.deps(), a, id); err != nil {
			t.Fatal(err)
		}
	}
	ids := e.runsFor(t, a)
	if len(ids) != 2 {
		t.Fatalf("%d runs", len(ids))
	}
	for _, id := range ids {
		r := e.getRun(t, id)
		if r.RequestedBy != "scheduler" || r.Status != "completed" || r.Snapshot["audit"] != 120 || r.Snapshot["activation"] != 180 {
			t.Fatalf("run %+v", r)
		}
	}
	// No policy: nothing happens at all.
	b := e.prefix + "wsNoPolicy"
	if err := Enforce(context.Background(), e.deps(), b, ""); err != nil || len(e.runsFor(t, b)) != 0 {
		t.Fatalf("no policy: %v %v", err, e.runsFor(t, b))
	}
}

func TestEnforceCompletedRunIsNoop(t *testing.T) {
	e := setupEnf(t)
	v := loadVectors(t, e.wsMap())
	e.seed(t, v, vecNow)
	a := e.prefix + "wsA"
	e.putPolicy(t, a, true, false, minimumDays)
	before := e.totals(t, a)
	for _, st := range []string{"completed", "cancelled"} {
		run := e.putRun(t, a, st, "manual", minimumDays)
		if err := Enforce(context.Background(), e.deps(), a, run); err != nil {
			t.Fatal(err)
		}
		if r := e.getRun(t, run); r.Status != st || r.Started {
			t.Fatalf("%s run touched: %+v", st, r)
		}
	}
	if !equalMaps(e.totals(t, a), before) {
		t.Fatal("rows deleted by a no-op")
	}
}

func TestEnforceFailureMarksFailedAndReturnsError(t *testing.T) {
	e := setupEnf(t)
	a := e.prefix + "wsA"
	e.putPolicy(t, a, true, false, minimumDays)
	run := e.putRun(t, a, "pending", "manual", map[string]int{"audit": 90}) // snapshot missing categories
	d := e.deps()
	err := Enforce(context.Background(), d, a, run)
	r := e.getRun(t, run)
	// An unusable snapshot (or a cancelled context) must fail the run, never complete it.
	if err == nil || r.Status != "failed" || r.Error == "" || !r.Finished {
		t.Fatalf("err=%v run=%+v", err, r)
	}
	// Cancelled context mid-run.
	e.putPolicy(t, a, true, false, minimumDays)
	v := loadVectors(t, e.wsMap())
	e.seed(t, v, vecNow)
	run2 := e.putRun(t, a, "pending", "manual", minimumDays)
	ctx, cancel := context.WithCancel(context.Background())
	d.BatchRows = 2
	d.AfterBatch = func(string, int64) { cancel() }
	err = Enforce(ctx, d, a, run2)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if r := e.getRun(t, run2); r.Status != "failed" || len(r.Error) > 1000 {
		t.Fatalf("run2 %+v", r)
	}
}

func TestEnforceReschedulesNextDay(t *testing.T) {
	e := setupEnf(t)
	a := e.prefix + "wsA"
	e.putPolicy(t, a, true, false, minimumDays)
	run := e.putRun(t, a, "pending", "scheduler", minimumDays)
	if err := Enforce(context.Background(), e.deps(), a, run); err != nil {
		t.Fatal(err)
	}
	var key string
	var due time.Time
	if err := e.owner.QueryRow(context.Background(),
		`SELECT fire_key, next_run_at FROM jobs WHERE type='retention_enforce' AND status='pending' AND fire_key LIKE $1`, "retention:"+a+":%").Scan(&key, &due); err != nil {
		t.Fatalf("no rescheduled job: %v", err)
	}
	next := time.Now().UTC().Add(24 * time.Hour)
	if !strings.HasPrefix(key, "retention:"+a+":") || due.Before(next.Add(-time.Hour)) || due.After(next.Add(time.Hour)) {
		t.Fatalf("job %s due %v (want ~%v)", key, due, next)
	}
	var enabled bool
	if err := e.owner.QueryRow(context.Background(), `SELECT enabled FROM retention_schedules WHERE workspace_id=$1`, a).Scan(&enabled); err != nil || !enabled {
		t.Fatalf("mirror %v %v", enabled, err)
	}
}

func TestEnforceNeverDeletesWithoutWorkspaceScope(t *testing.T) {
	e := setupEnf(t)
	v := loadVectors(t, e.wsMap())
	e.seed(t, v, vecNow)
	a, b := e.prefix+"wsA", e.prefix+"wsB"
	e.putPolicy(t, a, true, false, minimumDays)
	e.putPolicy(t, b, true, false, minimumDays)
	pool := testpg.NewAppPool(t, 1) // one connection: a leaked GUC would show up on the next use
	d := Deps{Pool: pool, Now: func() time.Time { return vecNow }, BatchRows: 3}
	beforeB := e.totals(t, b)
	run := e.putRun(t, a, "pending", "manual", minimumDays)
	if err := Enforce(context.Background(), d, a, run); err != nil {
		t.Fatal(err)
	}
	var ws string
	if err := pool.QueryRow(context.Background(), `SELECT coalesce(current_setting('app.workspace_id', true), '')`).Scan(&ws); err != nil || ws != "" {
		t.Fatalf("GUC leaked: %q %v", ws, err)
	}
	var seen int64
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_audit_events`).Scan(&seen); err != nil || seen != 0 {
		t.Fatalf("unscoped read sees %d rows (%v)", seen, err)
	}
	if !equalMaps(e.totals(t, b), beforeB) {
		t.Fatal("workspace B touched")
	}
	// Enforce with an empty workspace is refused outright.
	if err := Enforce(context.Background(), d, "", ""); err == nil {
		t.Fatal("empty workspace accepted")
	}
}

// --- Reconcile ---

func (e *env) putJob(t *testing.T, ws, status string) int64 {
	t.Helper()
	var id int64
	if err := e.owner.QueryRow(context.Background(),
		`INSERT INTO jobs (type,status,payload,executor,fire_key) VALUES ('retention_enforce',$1,$2::json,'python','retention:'||$3||':2020-01-01') RETURNING id`,
		status, `{"workspace_id": "`+ws+`"}`, ws).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.owner.Exec(context.Background(), `DELETE FROM jobs WHERE id=$1`, id) })
	return id
}

func payloadOf(ws, run string) []byte {
	m := map[string]string{"workspace_id": ws}
	if run != "" {
		m["run_id"] = run
	}
	b, _ := json.Marshal(m)
	return b
}

func (e *env) job(t *testing.T, id int64) (status, errText, payload string, completed bool) {
	t.Helper()
	var er *string
	if err := e.owner.QueryRow(context.Background(), `SELECT status, error, payload::text, completed_at IS NOT NULL FROM jobs WHERE id=$1`, id).Scan(&status, &er, &payload, &completed); err != nil {
		t.Fatal(err)
	}
	if er != nil {
		errText = *er
	}
	return
}

func TestReconcileVectorsMatchPython(t *testing.T) {
	ctx := context.Background()
	t.Run("retry", func(t *testing.T) {
		e := setupEnf(t)
		a := e.prefix + "wsA"
		e.putPolicy(t, a, true, false, minimumDays)
		run := e.putRun(t, a, "running", "manual", minimumDays)
		if _, err := e.owner.Exec(ctx, `UPDATE retention_runs SET finished_at=now() WHERE id=$1`, run); err != nil {
			t.Fatal(err)
		}
		job := e.putJob(t, a, "processing")
		if err := Reconcile(ctx, e.deps(), job, payloadOf(a, run), "boom", true); err != nil {
			t.Fatal(err)
		}
		r := e.getRun(t, run)
		if r.Status != "pending" || r.Error != "Queue retry scheduled: boom" || r.Finished {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("final reschedules when enabled", func(t *testing.T) {
		e := setupEnf(t)
		a := e.prefix + "wsA"
		e.putPolicy(t, a, true, false, minimumDays)
		run := e.putRun(t, a, "running", "manual", minimumDays)
		job := e.putJob(t, a, "processing")
		if err := Reconcile(ctx, e.deps(), job, payloadOf(a, run), "boom", false); err != nil {
			t.Fatal(err)
		}
		r := e.getRun(t, run)
		if r.Status != "failed" || r.Error != "Final failure: boom" || !r.Finished {
			t.Fatalf("%+v", r)
		}
		if n := e.pending(t, a); n != 1 {
			t.Fatalf("%d pending jobs, want 1 rescheduled", n)
		}
	})
	t.Run("final disabled does not reschedule", func(t *testing.T) {
		e := setupEnf(t)
		a := e.prefix + "wsA"
		e.putPolicy(t, a, false, false, minimumDays)
		run := e.putRun(t, a, "running", "manual", minimumDays)
		job := e.putJob(t, a, "processing")
		if err := Reconcile(ctx, e.deps(), job, payloadOf(a, run), "boom", false); err != nil {
			t.Fatal(err)
		}
		if r := e.getRun(t, run); r.Status != "failed" || e.pending(t, a) != 0 {
			t.Fatalf("%+v pending %d", r, e.pending(t, a))
		}
	})
	t.Run("legal hold cancels run and pending job", func(t *testing.T) {
		e := setupEnf(t)
		a := e.prefix + "wsA"
		e.putPolicy(t, a, true, true, minimumDays)
		run := e.putRun(t, a, "running", "manual", minimumDays)
		job := e.putJob(t, a, "pending")
		processing := e.putJob(t, a, "processing")
		// distinct fire key for the second job
		_, _ = e.owner.Exec(ctx, `UPDATE jobs SET fire_key=fire_key||'x' WHERE id=$1`, processing)
		if err := Reconcile(ctx, e.deps(), job, payloadOf(a, run), "boom", true); err != nil {
			t.Fatal(err)
		}
		r := e.getRun(t, run)
		if r.Status != "cancelled" || r.Error != "Legal hold enabled during retention enforcement" || !r.Finished {
			t.Fatalf("%+v", r)
		}
		st, er, _, done := e.job(t, job)
		if st != "cancelled" || er != r.Error || !done {
			t.Fatalf("job %s %q %v", st, er, done)
		}
		if err := Reconcile(ctx, e.deps(), processing, payloadOf(a, run), "boom", true); err != nil {
			t.Fatal(err)
		}
		if st, _, _, _ := e.job(t, processing); st != "processing" {
			t.Fatalf("non-pending job changed to %s", st)
		}
	})
	t.Run("completed or cancelled run untouched", func(t *testing.T) {
		e := setupEnf(t)
		a := e.prefix + "wsA"
		e.putPolicy(t, a, true, false, minimumDays)
		for _, st := range []string{"completed", "cancelled"} {
			run := e.putRun(t, a, st, "manual", minimumDays)
			job := e.putJob(t, a, "processing")
			_, _ = e.owner.Exec(ctx, `UPDATE jobs SET fire_key=fire_key||$2 WHERE id=$1`, job, st)
			if err := Reconcile(ctx, e.deps(), job, payloadOf(a, run), "boom", false); err != nil {
				t.Fatal(err)
			}
			if r := e.getRun(t, run); r.Status != st || r.Error != "" {
				t.Fatalf("%s %+v", st, r)
			}
		}
	})
	t.Run("missing run is created and linked to the job", func(t *testing.T) {
		e := setupEnf(t)
		a := e.prefix + "wsA"
		e.putPolicy(t, a, true, false, map[string]int{"audit": 200})
		job := e.putJob(t, a, "processing")
		if err := Reconcile(ctx, e.deps(), job, payloadOf(a, "gone"), "boom", true); err != nil {
			t.Fatal(err)
		}
		ids := e.runsFor(t, a)
		if len(ids) != 1 {
			t.Fatalf("%d runs", len(ids))
		}
		r := e.getRun(t, ids[0])
		if r.RequestedBy != "scheduler" || r.Status != "pending" || r.Error != "Queue retry scheduled: boom" || r.Snapshot["audit"] != 200 {
			t.Fatalf("%+v", r)
		}
		_, _, payload, _ := e.job(t, job)
		var got map[string]string
		_ = json.Unmarshal([]byte(payload), &got)
		if got["run_id"] != ids[0] || got["workspace_id"] != a {
			t.Fatalf("payload %s", payload)
		}
		// No policy: default days snapshot.
		b := e.prefix + "wsNoPolicy"
		j2 := e.putJob(t, b, "processing")
		if err := Reconcile(ctx, e.deps(), j2, payloadOf(b, ""), "x", false); err != nil {
			t.Fatal(err)
		}
		ids = e.runsFor(t, b)
		if len(ids) != 1 {
			t.Fatalf("%d runs w/o policy", len(ids))
		}
		if r := e.getRun(t, ids[0]); r.Snapshot["audit"] != 365 || r.Status != "failed" || e.pending(t, b) != 0 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("message truncation and default", func(t *testing.T) {
		e := setupEnf(t)
		a := e.prefix + "wsA"
		e.putPolicy(t, a, false, false, minimumDays)
		run := e.putRun(t, a, "running", "manual", minimumDays)
		job := e.putJob(t, a, "processing")
		if err := Reconcile(ctx, e.deps(), job, payloadOf(a, run), strings.Repeat("é", 1500), true); err != nil {
			t.Fatal(err)
		}
		if r := e.getRun(t, run); r.Error != "Queue retry scheduled: "+strings.Repeat("é", 1000) {
			t.Fatalf("len %d", len([]rune(r.Error)))
		}
		if err := Reconcile(ctx, e.deps(), job, payloadOf(a, run), "", true); err != nil {
			t.Fatal(err)
		}
		if r := e.getRun(t, run); r.Error != "Queue retry scheduled: retention enforcement failed" {
			t.Fatalf("%q", r.Error)
		}
	})
	t.Run("empty workspace errors", func(t *testing.T) {
		e := setupEnf(t)
		for _, p := range []string{`{}`, `{"workspace_id": ""}`, `{"workspace_id": "   "}`, `{"workspace_id": null}`, `not json`, ``} {
			err := Reconcile(ctx, e.deps(), 1, []byte(p), "x", true)
			if err == nil {
				t.Fatalf("payload %q accepted", p)
			}
		}
		if err := Reconcile(ctx, e.deps(), 1, []byte(`{}`), "x", true); err == nil || err.Error() != "retention failure payload requires workspace_id" {
			t.Fatalf("err %v", err)
		}
	})
}

func equalMaps(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func sumCounts(c Counts) (n int64) {
	for _, v := range c {
		n += v
	}
	return
}

var _ = tenancy.InWorkspace
var _ pgx.Tx
