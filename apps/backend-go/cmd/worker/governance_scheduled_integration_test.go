//go:build integration

package main

import (
	"context"
	"encoding/json"
	"testing"
)

// The schedulers (Python enqueue_job_once and Go SchedulePolicy) write jobs with a NULL
// jobs.workspace_id; the handlers must resolve the tenant from the payload.

func TestWorkerRunsScheduledRetentionJobWithoutJobWorkspace(t *testing.T) {
	g := setupGov(t)
	ctx := context.Background()
	days, _ := json.Marshal(govDays)
	if _, err := g.owner.Exec(ctx, `INSERT INTO retention_policies (workspace_id,enabled,legal_hold,retention_days) VALUES ($1,true,false,$2)`, g.ws, days); err != nil {
		t.Fatal(err)
	}
	key := "retention:" + g.ws + ":2099-01-01"
	payload, _ := json.Marshal(map[string]string{"workspace_id": g.ws, "fire_key": key})
	var job int64
	if err := g.owner.QueryRow(ctx, `INSERT INTO jobs (executor,type,payload,priority,fire_key,status,created_at,next_run_at,max_retries,retry_count)
		VALUES ('go','retention_enforce',$1,1,$2,'pending',now() AT TIME ZONE 'UTC',now() AT TIME ZONE 'UTC',3,0) RETURNING id`, string(payload), key).Scan(&job); err != nil {
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

func TestWorkerRunsPythonScheduledPlaybookJobWithoutJobWorkspace(t *testing.T) {
	g := setupGov(t)
	pb, aud := g.playbookFixture(t)
	ctx := context.Background()
	if _, err := g.owner.Exec(ctx, `UPDATE research_playbooks SET enabled=true, schedule_audience_id=$3, schedule_interval_minutes=60 WHERE workspace_id=$1 AND id=$2`, g.ws, pb.ID, aud); err != nil {
		t.Fatal(err)
	}
	key := "playbook_schedule:" + pb.ID + ":2099-01-01T00:00:00+00:00"
	payload, _ := json.Marshal(map[string]string{"workspace_id": g.ws, "playbook_id": pb.ID})
	var job int64
	if err := g.owner.QueryRow(ctx, `INSERT INTO jobs (executor,type,payload,priority,fire_key,status,created_at,next_run_at,max_retries,retry_count)
		VALUES ('go','research_playbook_schedule',$1,1,$2,'pending',now() AT TIME ZONE 'UTC',now() AT TIME ZONE 'UTC',3,0) RETURNING id`, string(payload), key).Scan(&job); err != nil {
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
