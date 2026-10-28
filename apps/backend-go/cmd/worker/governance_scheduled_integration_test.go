//go:build integration

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The Go schedulers (SchedulePolicy, schedboot, EnqueueOnce) write jobs.workspace_id; the handlers take the
// tenant from the claim only (final security Low 1). A job without one (Python's enqueue_job_once left it NULL)
// is refused, whatever its payload and fire key say; schedboot re-enqueues the occurrence with a workspace.

func TestWorkerRunsScheduledRetentionJob(t *testing.T) {
	g := setupGov(t)
	ctx := context.Background()
	days, _ := json.Marshal(govDays)
	if _, err := g.owner.Exec(ctx, `INSERT INTO retention_policies (workspace_id,enabled,legal_hold,retention_days) VALUES ($1,true,false,$2)`, g.ws, days); err != nil {
		t.Fatal(err)
	}
	key := "retention:" + g.ws + ":2099-01-01"
	payload, _ := json.Marshal(map[string]string{"workspace_id": g.ws, "fire_key": key})
	var job int64
	if err := g.owner.QueryRow(ctx, `INSERT INTO jobs (executor,type,payload,workspace_id,priority,fire_key,status,created_at,next_run_at,max_retries,retry_count)
		VALUES ('go','retention_enforce',$1,$3,1,$2,'pending',now() AT TIME ZONE 'UTC',now() AT TIME ZONE 'UTC',3,0) RETURNING id`, string(payload), key, g.ws).Scan(&job); err != nil {
		t.Fatal(err)
	}
	g.runGov(t, govConfig(g.ws), governanceOpts{})
	if s, e := g.jobStatus(t, job); s != "completed" {
		t.Fatalf("job %s (%s)", s, e)
	}
	var runs int
	var st, by string
	if err := g.owner.QueryRow(ctx, `SELECT count(*), coalesce(max(status),''), coalesce(max(requested_by),'') FROM retention_runs WHERE workspace_id=$1`, g.ws).Scan(&runs, &st, &by); err != nil || runs != 1 || st != "completed" || by != "scheduler" {
		t.Fatalf("runs=%d status=%q by=%q err=%v", runs, st, by, err)
	}
	var next int
	if err := g.owner.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE type='retention_enforce' AND status='pending' AND starts_with(fire_key,$1) AND fire_key<>$2`, "retention:"+g.ws+":", key).Scan(&next); err != nil || next != 1 {
		t.Fatalf("next occurrences=%d err=%v", next, err)
	}
}

func TestWorkerRejectsScheduledRetentionJobForeignWorkspace(t *testing.T) {
	g := setupGov(t)
	ctx := context.Background()
	payload, _ := json.Marshal(map[string]string{"workspace_id": g.ws + "-other"})
	var job int64
	if err := g.owner.QueryRow(ctx, `INSERT INTO jobs (type,payload,status,priority,created_at,retry_count,max_retries,workspace_id,executor)
		VALUES ('retention_enforce',$1,'pending',0,now() AT TIME ZONE 'UTC',0,0,$2,'go') RETURNING id`, string(payload), g.ws).Scan(&job); err != nil {
		t.Fatal(err)
	}
	g.runGov(t, govConfig(g.ws), governanceOpts{})
	if s, _ := g.jobStatus(t, job); s != "failed" {
		t.Fatalf("job %s, want failed", s)
	}
}

func TestWorkerRunsScheduledPlaybookJob(t *testing.T) {
	g := setupGov(t)
	pb, aud := g.playbookFixture(t)
	ctx := context.Background()
	if _, err := g.owner.Exec(ctx, `UPDATE research_playbooks SET enabled=true, schedule_audience_id=$3, schedule_interval_minutes=60 WHERE workspace_id=$1 AND id=$2`, g.ws, pb.ID, aud); err != nil {
		t.Fatal(err)
	}
	key := "playbook_schedule:" + pb.ID + ":2099-01-01T00:00:00+00:00"
	payload, _ := json.Marshal(map[string]string{"workspace_id": g.ws, "playbook_id": pb.ID})
	var job int64
	if err := g.owner.QueryRow(ctx, `INSERT INTO jobs (executor,type,payload,workspace_id,priority,fire_key,status,created_at,next_run_at,max_retries,retry_count)
		VALUES ('go','research_playbook_schedule',$1,$3,1,$2,'pending',now() AT TIME ZONE 'UTC',now() AT TIME ZONE 'UTC',3,0) RETURNING id`, string(payload), key, g.ws).Scan(&job); err != nil {
		t.Fatal(err)
	}
	g.runGov(t, govConfig(g.ws), governanceOpts{researcher: &govRes{}})
	if s, e := g.jobStatus(t, job); s != "completed" {
		t.Fatalf("schedule job %s (%s)", s, e)
	}
	var runs int
	if err := g.owner.QueryRow(ctx, `SELECT count(*) FROM playbook_runs WHERE workspace_id=$1 AND playbook_id=$2 AND requested_by='scheduler'`, g.ws, pb.ID).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("runs=%d err=%v", runs, err)
	}
	var next int
	if err := g.owner.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE type='research_playbook_schedule' AND status='pending' AND payload->>'playbook_id'=$1`, pb.ID).Scan(&next); err != nil || next != 1 {
		t.Fatalf("next schedule jobs=%d err=%v", next, err)
	}
}

// otherWorkspace creates a second real workspace (with an enabled retention policy) to forge a payload with.
func (g *govEnv) otherWorkspace(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	ws := g.ws + "-victim"
	if _, err := g.owner.Exec(ctx, `INSERT INTO workspaces (id,name,slug,description,icon,created_at,updated_at) VALUES ($1::text,$1::text,$1::text,'','',1,1)`, ws); err != nil {
		t.Fatal(err)
	}
	days, _ := json.Marshal(govDays)
	if _, err := g.owner.Exec(ctx, `INSERT INTO retention_policies (workspace_id,enabled,legal_hold,retention_days) VALUES ($1,true,false,$2)`, ws, days); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = g.owner.Exec(c, `DELETE FROM retention_runs WHERE workspace_id=$1`, ws)
		_, _ = g.owner.Exec(c, `DELETE FROM retention_policies WHERE workspace_id=$1`, ws)
		_, _ = g.owner.Exec(c, `DELETE FROM jobs WHERE payload::text LIKE '%'||$1||'%'`, ws)
		_, _ = g.owner.Exec(c, `DELETE FROM workspaces WHERE id=$1`, ws)
	})
	return ws
}

func (g *govEnv) emptyWSJob(t *testing.T, typ, payload string, key any) int64 {
	t.Helper()
	var job int64
	if err := g.owner.QueryRow(context.Background(), `INSERT INTO jobs (executor,type,payload,priority,fire_key,status,created_at,next_run_at,max_retries,retry_count)
		VALUES ('go',$1,$2,1,$3,'pending',now() AT TIME ZONE 'UTC',now() AT TIME ZONE 'UTC',0,0) RETURNING id`, typ, payload, key).Scan(&job); err != nil {
		t.Fatal(err)
	}
	return job
}

// wsJob is a scheduled job as the Go schedulers write it: jobs.workspace_id set.
func (g *govEnv) wsJob(t *testing.T, typ, payload string, key any) int64 {
	t.Helper()
	var job int64
	if err := g.owner.QueryRow(context.Background(), `INSERT INTO jobs (executor,type,payload,workspace_id,priority,fire_key,status,created_at,next_run_at,max_retries,retry_count)
		VALUES ('go',$1,$2,$4,1,$3,'pending',now() AT TIME ZONE 'UTC',now() AT TIME ZONE 'UTC',0,0) RETURNING id`, typ, payload, key, g.ws).Scan(&job); err != nil {
		t.Fatal(err)
	}
	return job
}

// Final security Low 1: a well-formed scheduled job (payload, fire key and policy all consistent) without
// jobs.workspace_id is refused; nothing runs.
func TestWorkerRefusesScheduledJobsWithoutJobWorkspace(t *testing.T) {
	g := setupGov(t)
	g.enabledPolicy(t, g.ws)
	pb, aud := g.playbookFixture(t)
	ctx := context.Background()
	if _, err := g.owner.Exec(ctx, `UPDATE research_playbooks SET enabled=true, schedule_audience_id=$3, schedule_interval_minutes=60 WHERE workspace_id=$1 AND id=$2`, g.ws, pb.ID, aud); err != nil {
		t.Fatal(err)
	}
	rkey := "retention:" + g.ws + ":2099-06-01"
	rp, _ := json.Marshal(map[string]string{"workspace_id": g.ws, "fire_key": rkey})
	pp, _ := json.Marshal(map[string]string{"workspace_id": g.ws, "playbook_id": pb.ID})
	jobs := map[string]int64{
		"retention": g.emptyWSJob(t, "retention_enforce", string(rp), rkey),
		"playbook":  g.emptyWSJob(t, "research_playbook_schedule", string(pp), "playbook_schedule:"+pb.ID+":2099-06-01T00:00:00+00:00"),
	}
	g.runGov(t, govConfig(g.ws), governanceOpts{researcher: &govRes{}})
	for name, id := range jobs {
		if s, _ := g.jobStatus(t, id); s != "failed" {
			t.Errorf("%s: job %s, want failed", name, s)
		}
	}
	var runs int
	if err := g.owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM retention_runs WHERE workspace_id=$1) + (SELECT count(*) FROM playbook_runs WHERE workspace_id=$1)`, g.ws).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("runs=%d err=%v", runs, err)
	}
}

func TestWorkerRejectsForgedEmptyWorkspaceRetentionJobs(t *testing.T) {
	g := setupGov(t)
	victim := g.otherWorkspace(t)
	days, _ := json.Marshal(govDays)
	if _, err := g.owner.Exec(context.Background(), `INSERT INTO retention_policies (workspace_id,enabled,legal_hold,retention_days) VALUES ($1,true,false,$2)`, g.ws, days); err != nil {
		t.Fatal(err)
	}
	forgedPayload, _ := json.Marshal(map[string]string{"workspace_id": victim})
	runPayload, _ := json.Marshal(map[string]string{"workspace_id": g.ws, "run_id": "r1"})
	okPayload, _ := json.Marshal(map[string]string{"workspace_id": g.ws})
	jobs := map[string]int64{
		"payload names another workspace": g.emptyWSJob(t, "retention_enforce", string(forgedPayload), "retention:"+g.ws+":2099-01-01"),
		"no fire key":                     g.emptyWSJob(t, "retention_enforce", string(okPayload), nil),
		"malformed fire key":              g.emptyWSJob(t, "retention_enforce", string(okPayload), "retention-run:"+g.ws),
		"run_id without workspace":        g.emptyWSJob(t, "retention_enforce", string(runPayload), "retention:"+g.ws+":2099-01-02"),
	}
	g.runGov(t, govConfig(g.ws), governanceOpts{})
	for name, id := range jobs {
		if s, _ := g.jobStatus(t, id); s != "failed" {
			t.Errorf("%s: job %s, want failed", name, s)
		}
	}
	var runs int
	if err := g.owner.QueryRow(context.Background(), `SELECT count(*) FROM retention_runs WHERE workspace_id IN ($1,$2)`, g.ws, victim).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("runs=%d err=%v", runs, err)
	}
}

func TestWorkerRejectsForgedEmptyWorkspacePlaybookJobs(t *testing.T) {
	g := setupGov(t)
	pb, aud := g.playbookFixture(t)
	victim := g.otherWorkspace(t)
	ctx := context.Background()
	if _, err := g.owner.Exec(ctx, `UPDATE research_playbooks SET enabled=true, schedule_audience_id=$3, schedule_interval_minutes=60 WHERE workspace_id=$1 AND id=$2`, g.ws, pb.ID, aud); err != nil {
		t.Fatal(err)
	}
	key := "playbook_schedule:" + pb.ID + ":2099-01-01T00:00:00+00:00"
	forged, _ := json.Marshal(map[string]string{"workspace_id": victim, "playbook_id": pb.ID})
	wrongKey, _ := json.Marshal(map[string]string{"workspace_id": g.ws, "playbook_id": pb.ID})
	runPayload, _ := json.Marshal(map[string]string{"workspace_id": g.ws, "run_id": "r1"})
	jobs := map[string]int64{
		"playbook of another workspace": g.emptyWSJob(t, "research_playbook_schedule", string(forged), key),
		"fire key of another playbook":  g.emptyWSJob(t, "research_playbook_schedule", string(wrongKey), "playbook_schedule:other-pb:2099-01-01T00:00:00+00:00"),
		"no fire key":                   g.emptyWSJob(t, "research_playbook_schedule", string(wrongKey), nil),
		"run job without workspace":     g.emptyWSJob(t, "research_playbook_run", string(runPayload), "playbook:r1"),
	}
	g.runGov(t, govConfig(g.ws), governanceOpts{researcher: &govRes{}})
	for name, id := range jobs {
		if s, _ := g.jobStatus(t, id); s != "failed" {
			t.Errorf("%s: job %s, want failed", name, s)
		}
	}
	var runs int
	if err := g.owner.QueryRow(ctx, `SELECT count(*) FROM playbook_runs WHERE workspace_id IN ($1,$2)`, g.ws, victim).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("runs=%d err=%v", runs, err)
	}
}

func (g *govEnv) schedulerRun(t *testing.T, ws, status, by string) string {
	t.Helper()
	var id string
	if err := g.owner.QueryRow(context.Background(), `INSERT INTO retention_runs (id,workspace_id,status,requested_by,policy_snapshot,deleted_counts)
		VALUES (gen_random_uuid()::text,$1,$2,$3,$4::json,'{}') RETURNING id`, ws, status, by, snapshotJSON()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (g *govEnv) enabledPolicy(t *testing.T, ws string) {
	t.Helper()
	days, _ := json.Marshal(govDays)
	if _, err := g.owner.Exec(context.Background(), `INSERT INTO retention_policies (workspace_id,enabled,legal_hold,retention_days) VALUES ($1,true,false,$2) ON CONFLICT DO NOTHING`, ws, days); err != nil {
		t.Fatal(err)
	}
}

// N1: the state after a failed attempt of a scheduled job:
// reconcile created a scheduler run, set it pending for the retry and wrote its run_id into
// the payload. The retry must complete the run and enqueue the next occurrence.
func TestWorkerRetriesReconciledScheduledRetentionJob(t *testing.T) {
	g := setupGov(t)
	g.enabledPolicy(t, g.ws)
	run := g.schedulerRun(t, g.ws, "pending", "scheduler")
	key := "retention:" + g.ws + ":2099-01-01"
	payload, _ := json.Marshal(map[string]string{"workspace_id": g.ws, "fire_key": key, "run_id": run})
	job := g.wsJob(t, "retention_enforce", string(payload), key)
	g.runGov(t, govConfig(g.ws), governanceOpts{})
	if s, e := g.jobStatus(t, job); s != "completed" {
		t.Fatalf("job %s (%s)", s, e)
	}
	if s, e := g.runStatus(t, run); s != "completed" {
		t.Fatalf("run %s (%s)", s, e)
	}
	var next int
	if err := g.owner.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE type='retention_enforce' AND status='pending' AND starts_with(fire_key,$1) AND fire_key<>$2`, "retention:"+g.ws+":", key).Scan(&next); err != nil || next != 1 {
		t.Fatalf("next occurrences=%d err=%v", next, err)
	}
}

func TestWorkerRejectsBadRunIDOnEmptyWorkspaceRetentionJob(t *testing.T) {
	g := setupGov(t)
	g.enabledPolicy(t, g.ws)
	victim := g.otherWorkspace(t)
	mk := func(i int, run string) int64 {
		key := "retention:" + g.ws + ":2099-02-0" + string(rune('1'+i))
		p, _ := json.Marshal(map[string]string{"workspace_id": g.ws, "fire_key": key, "run_id": run})
		return g.emptyWSJob(t, "retention_enforce", string(p), key)
	}
	jobs := map[string]int64{
		"unknown run":     mk(0, "no-such-run"),
		"other workspace": mk(1, g.schedulerRun(t, victim, "pending", "scheduler")),
		"manual run":      mk(2, g.schedulerRun(t, g.ws, "pending", "7")),
		"terminal run":    mk(3, g.schedulerRun(t, g.ws, "completed", "scheduler")),
	}
	g.runGov(t, govConfig(g.ws), governanceOpts{})
	for name, id := range jobs {
		if s, _ := g.jobStatus(t, id); s != "failed" {
			t.Errorf("%s: job %s, want failed", name, s)
		}
	}
	var started int
	if err := g.owner.QueryRow(context.Background(), `SELECT count(*) FROM retention_runs WHERE workspace_id IN ($1,$2) AND status IN ('running','completed') AND requested_by <> 'scheduler'`, g.ws, victim).Scan(&started); err != nil || started != 0 {
		t.Fatalf("started=%d err=%v", started, err)
	}
}

// N3: a scheduled job for a missing or disabled policy ends completed (no run, nothing purged),
// not failed. Fails if the policy check is removed (Enforce would then create and run a run).
func TestWorkerCompletesScheduledRetentionJobWithoutEnabledPolicy(t *testing.T) {
	for _, tc := range []string{"missing", "disabled"} {
		t.Run(tc, func(t *testing.T) {
			g := setupGov(t)
			if tc == "disabled" {
				days, _ := json.Marshal(govDays)
				if _, err := g.owner.Exec(context.Background(), `INSERT INTO retention_policies (workspace_id,enabled,legal_hold,retention_days) VALUES ($1,false,false,$2)`, g.ws, days); err != nil {
					t.Fatal(err)
				}
			}
			key := "retention:" + g.ws + ":2099-03-01"
			p, _ := json.Marshal(map[string]string{"workspace_id": g.ws, "fire_key": key})
			job := g.wsJob(t, "retention_enforce", string(p), key)
			g.runGov(t, govConfig(g.ws), governanceOpts{})
			if s, e := g.jobStatus(t, job); s != "completed" {
				t.Fatalf("job %s (%s)", s, e)
			}
			var runs int
			if err := g.owner.QueryRow(context.Background(), `SELECT count(*) FROM retention_runs WHERE workspace_id=$1`, g.ws).Scan(&runs); err != nil || runs != 0 {
				t.Fatalf("runs=%d err=%v", runs, err)
			}
		})
	}
}

// N3/N4 playbooks: an inactive playbook's schedule job completes (HandleSchedule removes the
// schedule, as Python); a fire key with a bad tail is a permanent failure.
func TestWorkerPlaybookScheduleJobInactiveAndBadTail(t *testing.T) {
	g := setupGov(t)
	pb, _ := g.playbookFixture(t) // enabled but no schedule audience: inactive
	p, _ := json.Marshal(map[string]string{"workspace_id": g.ws, "playbook_id": pb.ID})
	inactive := g.wsJob(t, "research_playbook_schedule", string(p), "playbook_schedule:"+pb.ID+":2099-01-01T00:00:00+00:00")
	badTail := g.wsJob(t, "research_playbook_schedule", string(p), "playbook_schedule:"+pb.ID+":whatever")
	g.runGov(t, govConfig(g.ws), governanceOpts{researcher: &govRes{}})
	if s, e := g.jobStatus(t, inactive); s != "completed" {
		t.Errorf("inactive: %s (%s)", s, e)
	}
	if s, _ := g.jobStatus(t, badTail); s != "failed" {
		t.Errorf("bad tail: %s", s)
	}
}

// failDeletes makes every DELETE on signals fail (Enforce error) until the returned func is called.
func (g *govEnv) failDeletes(t *testing.T) (restore func()) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`CREATE OR REPLACE FUNCTION p11_fail_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'p11 forced delete failure'; END $$`,
		`DROP TRIGGER IF EXISTS p11_fail ON signals`,
		`CREATE TRIGGER p11_fail BEFORE DELETE ON signals FOR EACH ROW EXECUTE FUNCTION p11_fail_delete()`,
		`INSERT INTO signals (id,workspace_id,created_at) VALUES ('p11-old-'||$1::text,$1::text,1577836800)`,
	} {
		var err error
		if q[len(q)-1] == ')' && len(q) > 6 && q[:6] == "INSERT" {
			_, err = g.owner.Exec(ctx, q, g.ws)
		} else {
			_, err = g.owner.Exec(ctx, q)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	done := false
	restore = func() {
		if !done {
			done = true
			_, _ = g.owner.Exec(ctx, `DROP TRIGGER IF EXISTS p11_fail ON signals`)
		}
	}
	t.Cleanup(restore)
	return restore
}

func (g *govEnv) dueNow(t *testing.T, job int64) {
	t.Helper()
	if _, err := g.owner.Exec(context.Background(), `UPDATE jobs SET next_run_at = now() AT TIME ZONE 'UTC' WHERE id=$1 AND status='pending'`, job); err != nil {
		t.Fatal(err)
	}
}

// N1 on the real path: a scheduled job fails, the real failure
// hook links a run, the retries fail again (Enforce marks the run failed itself), and the final
// attempt exhausts the retries. The failure hook must still reconcile (run failed with the final
// message) and enqueue the next occurrence, never reject the job.
func TestWorkerScheduledRetentionFailureRetryExhaustionReschedules(t *testing.T) {
	g := setupGov(t)
	g.enabledPolicy(t, g.ws)
	g.failDeletes(t)
	key := "retention:" + g.ws + ":2099-04-01"
	p, _ := json.Marshal(map[string]string{"workspace_id": g.ws, "fire_key": key})
	var job int64
	if err := g.owner.QueryRow(context.Background(), `INSERT INTO jobs (executor,type,payload,workspace_id,priority,fire_key,status,created_at,next_run_at,max_retries,retry_count)
		VALUES ('go','retention_enforce',$1,$3,1,$2,'pending',now() AT TIME ZONE 'UTC',now() AT TIME ZONE 'UTC',2,0) RETURNING id`, string(p), key, g.ws).Scan(&job); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		g.runGov(t, govConfig(g.ws), governanceOpts{})
		if attempt < 3 {
			if s, e := g.jobStatus(t, job); s != "pending" {
				t.Fatalf("attempt %d: job %s (%s), want pending retry", attempt, s, e)
			}
			g.dueNow(t, job)
		}
	}
	if s, e := g.jobStatus(t, job); s != "failed" {
		t.Fatalf("job %s (%s), want failed after exhausting retries", s, e)
	}
	var runID string
	if err := g.owner.QueryRow(context.Background(), `SELECT payload->>'run_id' FROM jobs WHERE id=$1`, job).Scan(&runID); err != nil || runID == "" {
		t.Fatalf("no run linked: %q %v", runID, err)
	}
	if s, e := g.runStatus(t, runID); s != "failed" || !strings.HasPrefix(e, "Final failure:") {
		t.Fatalf("run %s %q", s, e)
	}
	var next int
	if err := g.owner.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE type='retention_enforce' AND status='pending' AND starts_with(fire_key,$1) AND fire_key<>$2`, "retention:"+g.ws+":", key).Scan(&next); err != nil || next != 1 {
		t.Fatalf("next occurrences=%d err=%v", next, err)
	}
}

// A failed scheduler run linked into the payload is admitted (Python's enforce re-runs any
// run that is not completed or cancelled); completed and cancelled ones are not.
func TestWorkerRetriesScheduledRetentionJobWithFailedRun(t *testing.T) {
	g := setupGov(t)
	g.enabledPolicy(t, g.ws)
	run := g.schedulerRun(t, g.ws, "failed", "scheduler")
	key := "retention:" + g.ws + ":2099-05-01"
	p, _ := json.Marshal(map[string]string{"workspace_id": g.ws, "fire_key": key, "run_id": run})
	job := g.wsJob(t, "retention_enforce", string(p), key)
	g.runGov(t, govConfig(g.ws), governanceOpts{})
	if s, e := g.jobStatus(t, job); s != "completed" {
		t.Fatalf("job %s (%s)", s, e)
	}
	if s, e := g.runStatus(t, run); s != "completed" {
		t.Fatalf("run %s (%s)", s, e)
	}
}
