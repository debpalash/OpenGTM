//go:build integration

package playbooks

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

var t10Now = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

func (e *env) deps() Deps { return Deps{Pool: e.pool, Now: func() time.Time { return t10Now }} }

// schedulable turns pb into an enabled, scheduled playbook in the database and returns it.
func (e *env) schedulable(t *testing.T, ws string, pb Playbook, aud string, minutes int) Playbook {
	t.Helper()
	e.sql(t, ws, `UPDATE research_playbooks SET enabled=true, schedule_audience_id=$3, schedule_interval_minutes=$4 WHERE workspace_id=$1 AND id=$2`, ws, pb.ID, aud, minutes)
	pb.Enabled, pb.ScheduleAudienceID, pb.ScheduleIntervalMinutes = true, &aud, &minutes
	return pb
}

type jobRow struct{ key, status, executor string }

func (e *env) jobs(t *testing.T, typ string, ws string) []jobRow {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT fire_key, status, executor FROM jobs WHERE workspace_id=$1 AND type=$2 ORDER BY id`, ws, typ)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []jobRow
	for rows.Next() {
		var r jobRow
		if err := rows.Scan(&r.key, &r.status, &r.executor); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (e *env) insertScheduleJob(t *testing.T, ws, key string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO jobs (executor,type,payload,workspace_id,priority,fire_key,status,created_at,next_run_at,max_retries,retry_count)
VALUES ('python','research_playbook_schedule','{}',$1,1,$2,'pending',(now() AT TIME ZONE 'UTC'),(now() AT TIME ZONE 'UTC'),3,0)`, ws, key); err != nil {
		t.Fatal(err)
	}
}

func (e *env) pendingKeys(t *testing.T, ws string) []string {
	var out []string
	for _, j := range e.jobs(t, "research_playbook_schedule", ws) {
		if j.status == "pending" {
			out = append(out, j.key)
		}
	}
	return out
}

type mirrorRow struct {
	enabled bool
	next    *time.Time
	found   bool
}

func (e *env) mirror(t *testing.T, id string) mirrorRow {
	t.Helper()
	var m mirrorRow
	rows, err := e.pool.Query(context.Background(), `SELECT enabled, next_run_at FROM playbook_schedules WHERE playbook_id=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		m.found = true
		if err := rows.Scan(&m.enabled, &m.next); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func (e *env) scheduleNext(t *testing.T, ws string, pb *Playbook) (*time.Time, error) {
	t.Helper()
	var next *time.Time
	err := e.do(t, ws, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		next, err = ScheduleNext(ctx, tx, pb, t10Now)
		return err
	})
	return next, err
}

func TestScheduleNextClampsInterval(t *testing.T) {
	e := setup(t)
	for minutes, want := range map[int]time.Duration{5: 15 * time.Minute, 60: time.Hour, 99999: 10080 * time.Minute} {
		pb := e.schedulable(t, e.ws, e.mkPlaybook(t, e.ws, "clamp"+time.Duration(minutes).String()), e.aud, minutes)
		next, err := e.scheduleNext(t, e.ws, &pb)
		if err != nil || next == nil {
			t.Fatalf("%d: %v %v", minutes, next, err)
		}
		if !next.Equal(t10Now.Add(want)) {
			t.Fatalf("%d: next %v want %v", minutes, next, t10Now.Add(want))
		}
		if pb.NextRunAt == nil || !pb.NextRunAt.Equal(*next) {
			t.Fatalf("pb.NextRunAt %v", pb.NextRunAt)
		}
		var stored *time.Time
		e.in(t, e.ws, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT next_run_at FROM research_playbooks WHERE id=$1`, pb.ID).Scan(&stored)
		})
		if stored == nil || !stored.Equal(*next) {
			t.Fatalf("stored next_run_at %v want %v", stored, next)
		}
		m := e.mirror(t, pb.ID)
		if !m.found || !m.enabled || m.next == nil || !m.next.Equal(*next) {
			t.Fatalf("mirror %+v", m)
		}
		wantKey := "playbook_schedule:" + pb.ID + ":" + next.UTC().Format("2006-01-02T15:04:05") + "+00:00"
		found := false
		for _, k := range e.pendingKeys(t, e.ws) {
			found = found || k == wantKey
		}
		if !found {
			t.Fatalf("job %q missing in %v", wantKey, e.pendingKeys(t, e.ws))
		}
	}
}

func TestScheduleNextInactiveClearsMirror(t *testing.T) {
	e := setup(t)
	pb := e.schedulable(t, e.ws, e.mkPlaybook(t, e.ws, "inactive"), e.aud, 30)
	if next, err := e.scheduleNext(t, e.ws, &pb); err != nil || next == nil {
		t.Fatal(next, err)
	}
	cases := map[string]func(p *Playbook){
		"disabled":    func(p *Playbook) { p.Enabled = false },
		"no audience": func(p *Playbook) { p.ScheduleAudienceID = nil },
		"no interval": func(p *Playbook) { p.ScheduleIntervalMinutes = nil },
		"zero":        func(p *Playbook) { z := 0; p.ScheduleIntervalMinutes = &z },
	}
	for name, mut := range cases {
		p := e.schedulable(t, e.ws, pb, e.aud, 30)
		if _, err := e.scheduleNext(t, e.ws, &p); err != nil {
			t.Fatal(err)
		}
		mut(&p)
		next, err := e.scheduleNext(t, e.ws, &p)
		if err != nil || next != nil {
			t.Fatalf("%s: next %v err %v", name, next, err)
		}
		if p.NextRunAt != nil {
			t.Fatalf("%s: pb.NextRunAt %v", name, p.NextRunAt)
		}
		if m := e.mirror(t, pb.ID); !m.found || m.enabled || m.next != nil {
			t.Fatalf("%s: mirror %+v", name, m)
		}
		if k := e.pendingKeys(t, e.ws); len(k) != 0 {
			t.Fatalf("%s: pending %v", name, k)
		}
	}
}

func TestScheduleNextCancelsPendingOccurrence(t *testing.T) {
	e := setup(t)
	pb := e.schedulable(t, e.ws, e.mkPlaybook(t, e.ws, "cancel"), e.aud, 30)
	other := e.mkPlaybook(t, e.ws, "cancel-other")
	// a playbook whose id extends this one's must keep its occurrence ('a' vs 'a:b'),
	// as must the same-shaped key of another workspace and a LIKE-metachar lookalike.
	e.insertScheduleJob(t, e.ws, "playbook_schedule:"+pb.ID+":2026-06-01T00:00:00+00:00")
	e.insertScheduleJob(t, e.ws, "playbook_schedule:"+pb.ID+":bootstrap")
	e.insertScheduleJob(t, e.ws, "playbook_schedule:"+pb.ID+":b:2026-06-01T00:00:00+00:00")
	e.insertScheduleJob(t, e.ws, "playbook_schedule:"+pb.ID+"%:2026-06-01T00:00:00+00:00")
	e.insertScheduleJob(t, e.ws, "playbook_schedule:"+other.ID+":2026-06-01T00:00:00+00:00")
	e.insertScheduleJob(t, e.ws2, "playbook_schedule:"+pb.ID+":2026-06-02T00:00:00+00:00")
	if _, err := e.scheduleNext(t, e.ws, &pb); err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, j := range e.jobs(t, "research_playbook_schedule", e.ws) {
		status[j.key] = j.status
	}
	p := "playbook_schedule:" + pb.ID
	for k, want := range map[string]string{
		p + ":2026-06-01T00:00:00+00:00":                               "cancelled",
		p + ":bootstrap":                                               "cancelled",
		p + ":b:2026-06-01T00:00:00+00:00":                             "pending",
		p + "%:2026-06-01T00:00:00+00:00":                              "pending",
		"playbook_schedule:" + other.ID + ":2026-06-01T00:00:00+00:00": "pending",
		p + ":2026-06-15T12:30:00+00:00":                               "pending",
	} {
		if status[k] != want {
			t.Errorf("%s: %q want %q", k, status[k], want)
		}
	}
	if j := e.jobs(t, "research_playbook_schedule", e.ws2); len(j) != 1 || j[0].status != "pending" {
		t.Fatalf("foreign workspace job touched: %+v", j)
	}
}

func TestScheduleNextRejectsForeignTenant(t *testing.T) {
	e := setup(t)
	pb := e.schedulable(t, e.ws, e.mkPlaybook(t, e.ws, "foreign"), e.aud, 30)
	// pb belongs to ws but the transaction is ws2's tenant: the playbook row is
	// invisible there, so nothing may be written for it.
	if _, err := e.scheduleNext(t, e.ws2, &pb); err == nil {
		t.Fatal("expected an error for a playbook outside the transaction tenant")
	}
	if m := e.mirror(t, pb.ID); m.found {
		t.Fatalf("foreign mirror written %+v", m)
	}
	if k := e.pendingKeys(t, e.ws2); len(k) != 0 {
		t.Fatalf("jobs written %v", k)
	}
}

func (e *env) runs(t *testing.T, ws, pbID string) []Run {
	t.Helper()
	var out []Run
	e.in(t, ws, func(tx pgx.Tx) error {
		page, err := ListRuns(context.Background(), tx, ws, pbID, 100, 0, "")
		out = page.Items
		return err
	})
	return out
}

func TestHandleScheduleCreatesRunOnce(t *testing.T) {
	e := setup(t)
	pb := e.schedulable(t, e.ws, e.mkPlaybook(t, e.ws, "once"), e.aud, 30)
	d := e.deps()
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- HandleSchedule(context.Background(), d, e.ws, pb.ID) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	runs := e.runs(t, e.ws, pb.ID)
	if len(runs) != 1 {
		t.Fatalf("%d runs, want 1", len(runs))
	}
	r := runs[0]
	if r.Status != "pending" || r.MaxMembers != 100 || r.AudienceID != e.aud || r.PromptVersion != pb.Version {
		t.Fatalf("run %+v", r)
	}
	var by string
	e.in(t, e.ws, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT requested_by FROM playbook_runs WHERE id=$1`, r.ID).Scan(&by)
	})
	if by != "scheduler" {
		t.Fatalf("requested_by %q", by)
	}
	rj := e.jobs(t, "research_playbook_run", e.ws)
	if len(rj) != 1 || rj[0].key != "playbook:"+r.ID || rj[0].status != "pending" || rj[0].executor != "python" {
		t.Fatalf("run jobs %+v", rj)
	}
	if k := e.pendingKeys(t, e.ws); len(k) != 1 || k[0] != "playbook_schedule:"+pb.ID+":2026-06-15T12:30:00+00:00" {
		t.Fatalf("pending schedule jobs %v", k)
	}
	if m := e.mirror(t, pb.ID); !m.found || !m.enabled || m.next == nil || !m.next.Equal(t10Now.Add(30*time.Minute)) {
		t.Fatalf("mirror %+v", m)
	}
}

func TestHandleScheduleHonoursRunExecutor(t *testing.T) {
	e := setup(t)
	pb := e.schedulable(t, e.ws, e.mkPlaybook(t, e.ws, "exec"), e.aud, 30)
	d := e.deps()
	d.RunExecutor = func(ws string) string {
		if ws != e.ws {
			t.Errorf("executor asked for %q", ws)
		}
		return "go"
	}
	if err := HandleSchedule(context.Background(), d, e.ws, pb.ID); err != nil {
		t.Fatal(err)
	}
	if rj := e.jobs(t, "research_playbook_run", e.ws); len(rj) != 1 || rj[0].executor != "go" {
		t.Fatalf("%+v", rj)
	}
}

func TestHandleScheduleSkipsWhenRunActive(t *testing.T) {
	e := setup(t)
	pb := e.schedulable(t, e.ws, e.mkPlaybook(t, e.ws, "active"), e.aud, 30)
	for _, st := range []string{"pending", "running"} {
		e.mkRun(t, e.ws, pb, e.aud)
		e.sql(t, e.ws, `UPDATE playbook_runs SET status=$2 WHERE workspace_id=$1`, e.ws, st)
		if err := HandleSchedule(context.Background(), e.deps(), e.ws, pb.ID); err != nil {
			t.Fatal(err)
		}
		if n := len(e.runs(t, e.ws, pb.ID)); n != 1 {
			t.Fatalf("%s: %d runs", st, n)
		}
		e.sql(t, e.ws, `DELETE FROM playbook_runs WHERE workspace_id=$1`, e.ws)
	}
	// the chain continues even when no run was made.
	if k := e.pendingKeys(t, e.ws); len(k) != 1 {
		t.Fatalf("pending %v", k)
	}
	if rj := e.jobs(t, "research_playbook_run", e.ws); len(rj) != 0 {
		t.Fatalf("run jobs %+v", rj)
	}
}

func TestHandleScheduleRemovesWhenPlaybookDisabled(t *testing.T) {
	e := setup(t)
	d := e.deps()
	mk := func(name string, mut string) Playbook {
		pb := e.schedulable(t, e.ws, e.mkPlaybook(t, e.ws, name), e.aud, 30)
		if _, err := e.scheduleNext(t, e.ws, &pb); err != nil {
			t.Fatal(err)
		}
		if mut != "" {
			e.sql(t, e.ws, mut, e.ws, pb.ID)
		}
		return pb
	}
	for name, mut := range map[string]string{
		"disabled":    `UPDATE research_playbooks SET enabled=false WHERE workspace_id=$1 AND id=$2`,
		"no audience": `UPDATE research_playbooks SET schedule_audience_id=NULL WHERE workspace_id=$1 AND id=$2`,
	} {
		pb := mk("rm-"+name, mut)
		if err := HandleSchedule(context.Background(), d, e.ws, pb.ID); err != nil {
			t.Fatal(err)
		}
		if m := e.mirror(t, pb.ID); m.found {
			t.Fatalf("%s: mirror kept %+v", name, m)
		}
		for _, k := range e.pendingKeys(t, e.ws) {
			if len(k) > 0 && k[:len("playbook_schedule:"+pb.ID+":")] == "playbook_schedule:"+pb.ID+":" {
				t.Fatalf("%s: pending %q", name, k)
			}
		}
		if n := len(e.runs(t, e.ws, pb.ID)); n != 0 {
			t.Fatalf("%s: %d runs", name, n)
		}
	}
	// a playbook that is gone (or in another workspace) is removed the same way.
	gone := mk("rm-gone", "")
	e.sql(t, e.ws, `DELETE FROM research_playbooks WHERE workspace_id=$1 AND id=$2`, e.ws, gone.ID)
	if err := HandleSchedule(context.Background(), d, e.ws, gone.ID); err != nil {
		t.Fatal(err)
	}
	if m := e.mirror(t, gone.ID); m.found {
		t.Fatalf("gone: mirror kept")
	}
	if err := HandleSchedule(context.Background(), d, "", "x"); err == nil {
		t.Fatal("empty workspace must be rejected")
	}
	if err := HandleSchedule(context.Background(), d, e.ws, ""); err == nil {
		t.Fatal("empty playbook id must be rejected")
	}
}

func TestHandleScheduleRunAndEnqueueAtomic(t *testing.T) {
	e := setup(t)
	pb := e.schedulable(t, e.ws, e.mkPlaybook(t, e.ws, "atomic"), e.aud, 30)
	d := e.deps()
	d.RunExecutor = func(string) string { return "bogus" } // EnqueueRun rejects it
	err := HandleSchedule(context.Background(), d, e.ws, pb.ID)
	if err == nil {
		t.Fatal("expected enqueue failure")
	}
	if n := len(e.runs(t, e.ws, pb.ID)); n != 0 {
		t.Fatalf("run survived a failed enqueue: %d", n)
	}
	if rj := e.jobs(t, "research_playbook_run", e.ws); len(rj) != 0 {
		t.Fatalf("%+v", rj)
	}
	if k := e.pendingKeys(t, e.ws); len(k) != 0 {
		t.Fatalf("schedule enqueued despite failure: %v", k)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatal("wrong error")
	}
}

func TestRemoveScheduleExactAndTenantScoped(t *testing.T) {
	e := setup(t)
	pb := e.schedulable(t, e.ws, e.mkPlaybook(t, e.ws, "rmx"), e.aud, 30)
	if _, err := e.scheduleNext(t, e.ws, &pb); err != nil {
		t.Fatal(err)
	}
	e.insertScheduleJob(t, e.ws, "playbook_schedule:"+pb.ID+":b:2026-06-01T00:00:00+00:00")
	if err := e.do(t, e.ws, func(ctx context.Context, tx pgx.Tx) error { return RemoveSchedule(ctx, tx, pb.ID) }); err != nil {
		t.Fatal(err)
	}
	if m := e.mirror(t, pb.ID); m.found {
		t.Fatal("mirror kept")
	}
	if k := e.pendingKeys(t, e.ws); len(k) != 1 || k[0] != "playbook_schedule:"+pb.ID+":b:2026-06-01T00:00:00+00:00" {
		t.Fatalf("pending %v", k)
	}
	// another workspace's transaction cannot remove this playbook's mirror.
	if _, err := e.scheduleNext(t, e.ws, &pb); err != nil {
		t.Fatal(err)
	}
	if err := e.do(t, e.ws2, func(ctx context.Context, tx pgx.Tx) error { return RemoveSchedule(ctx, tx, pb.ID) }); err != nil {
		t.Fatal(err)
	}
	if m := e.mirror(t, pb.ID); !m.found {
		t.Fatal("foreign tenant removed the mirror")
	}
}

// BootstrapDue stays in the Python scheduler. Its occurrence keys (the mirror's
// next_run_at, or "bootstrap") must be recognised by the Go cancel and handler.
func TestBootstrapOccurrenceKeysAreHandledLikeSchedule(t *testing.T) {
	e := setup(t)
	pb := e.schedulable(t, e.ws, e.mkPlaybook(t, e.ws, "boot"), e.aud, 30)
	e.insertScheduleJob(t, e.ws, "playbook_schedule:"+pb.ID+":bootstrap")
	if err := HandleSchedule(context.Background(), e.deps(), e.ws, pb.ID); err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, j := range e.jobs(t, "research_playbook_schedule", e.ws) {
		status[j.key] = j.status
	}
	if status["playbook_schedule:"+pb.ID+":bootstrap"] != "cancelled" {
		t.Fatalf("%v", status)
	}
}
