package playbooksched

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/jobs/jobkit/jobtest"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

var frozen = time.Date(2026, 6, 15, 12, 0, 0, 123456000, time.UTC)

type env struct {
	*jobtest.Env
	worker *Worker
	spaces []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{Env: jobtest.New(t)}
	e.worker = NewWorker(e.App, nil)
	e.worker.Now = func() time.Time { return frozen }
	t.Cleanup(e.cleanup)
	return e
}

func (e *env) ws(label string) string {
	ws := jobtest.Unique("pbs-" + label)
	e.spaces = append(e.spaces, ws)
	return ws
}

func (e *env) cleanup() {
	ctx := context.Background()
	for _, ws := range e.spaces {
		for _, t := range []string{"playbook_runs", "research_playbooks", "audiences", "playbook_schedules"} {
			_, _ = e.Owner.Exec(ctx, "DELETE FROM "+t+" WHERE workspace_id = $1", ws)
		}
		_, _ = e.Owner.Exec(ctx, `DELETE FROM jobs WHERE type IN ($1, $2)
			AND (workspace_id = $3 OR payload->>'workspace_id' = $3)`, JobType, runJobType, ws)
	}
}

func (e *env) audience(ws, id string) {
	e.Exec(`INSERT INTO audiences (id, workspace_id, name, filters) VALUES ($1, $2, $1, '{}')`, id, ws)
}

func (e *env) playbook(ws, id string, enabled bool, aud *string, minutes *int, steps string) {
	e.Exec(`INSERT INTO research_playbooks (id, workspace_id, name, description, prompt_template, steps, output_format,
			max_steps, cell_budget_usd, version, enabled, schedule_audience_id, schedule_interval_minutes)
		VALUES ($1::text, $2, $1::text, '', 'prompt '||$1::text, $3::json, 'text', 4, 0.1, 5, $4, $5, $6)`, id, ws, steps, enabled, aud, minutes)
}

func ptr[T any](v T) *T { return &v }

func (e *env) tick(ws, playbook string) queue.Job {
	return e.Claimed(JobType, "", map[string]string{"workspace_id": ws, "playbook_id": playbook})
}

func TestTickCreatesRunItsJobAndTheNextOccurrence(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("a")
	e.audience(ws, "aud-"+ws)
	e.playbook(ws, "pb-"+ws, true, ptr("aud-"+ws), ptr(30), `[{"tool": "x"}]`)

	if err := e.worker.Handle(context.Background(), e.tick(ws, "pb-"+ws)); err != nil {
		t.Fatal(err)
	}
	var runID, status, prompt, by, steps string
	var ver, maxMembers int
	err := e.Owner.QueryRow(context.Background(), `SELECT id, status, prompt_snapshot, requested_by, steps_snapshot::jsonb::text,
		prompt_version, max_members FROM playbook_runs WHERE workspace_id = $1 AND playbook_id = $2`, ws, "pb-"+ws).
		Scan(&runID, &status, &prompt, &by, &steps, &ver, &maxMembers)
	if err != nil {
		t.Fatal(err)
	}
	if status != "pending" || prompt != "prompt pb-"+ws || by != "scheduler" || steps != `[{"tool": "x"}]` || ver != 5 || maxMembers != 100 {
		t.Errorf("run = %s %s %s %s %d %d", status, prompt, by, steps, ver, maxMembers)
	}
	// The run job: addressed to the Python executor's type, keyed by its run, tenant-stamped.
	if e.Count("jobs", `type = $1 AND workspace_id = $2 AND fire_key = $3 AND status = 'pending'
		AND payload->>'run_id' = $4 AND payload->>'workspace_id' = $2`, runJobType, ws, "playbook:"+runID, runID) != 1 {
		t.Error("expected one pending research_playbook_run job for the run")
	}
	// The next occurrence 30 minutes out, with Python's isoformat in the key.
	key := "playbook_schedule:pb-" + ws + ":2026-06-15T12:30:00.123456+00:00"
	if e.Count("jobs", `type = $1 AND fire_key = $2 AND status = 'pending' AND workspace_id IS NULL
		AND payload->>'workspace_id' = $3 AND payload->>'playbook_id' = $4 AND payload->>'fire_key' = $2`,
		JobType, key, ws, "pb-"+ws) != 1 {
		t.Errorf("expected one pending occurrence %s", key)
	}
	if e.Scalar(`SELECT next_run_at = '2026-06-15 12:30:00.123456+00'::timestamptz::timestamp FROM research_playbooks WHERE id = $1`, "pb-"+ws) != "true" {
		t.Error("playbook.next_run_at not set to now+30m")
	}
	if e.Scalar(`SELECT enabled FROM playbook_schedules WHERE playbook_id = $1`, "pb-"+ws) != "true" {
		t.Error("schedule mirror not enabled")
	}

	// A second tick finds the pending run and only reschedules.
	if err := e.worker.Handle(context.Background(), e.tick(ws, "pb-"+ws)); err != nil {
		t.Fatal(err)
	}
	if n := e.Count("playbook_runs", "workspace_id = $1", ws); n != 1 {
		t.Errorf("a second tick created another run: %d runs", n)
	}
	if n := e.Count("jobs", "type = $1 AND workspace_id = $2", runJobType, ws); n != 1 {
		t.Errorf("a second tick enqueued another run job: %d", n)
	}
	if e.Count("jobs", "type = $1 AND fire_key = $2 AND status = 'pending'", JobType, key) != 1 ||
		e.Count("jobs", "type = $1 AND fire_key = $2 AND status = 'cancelled'", JobType, key) != 1 {
		t.Error("the rescheduled occurrence must replace (cancel + recreate) the previous one")
	}
}

func TestNoUsableConfigurationRemovesTheSchedule(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("rm")
	e.audience(ws, "aud-"+ws)
	e.playbook(ws, "pb-off-"+ws, false, ptr("aud-"+ws), ptr(30), `[]`)
	e.playbook(ws, "pb-noaud-"+ws, true, nil, ptr(30), `[]`)
	for _, id := range []string{"pb-off-" + ws, "pb-noaud-" + ws, "pb-ghost-" + ws} {
		e.Exec(`INSERT INTO playbook_schedules (playbook_id, workspace_id, enabled) VALUES ($1, $2, true)`, id, ws)
		e.Exec(`INSERT INTO jobs (type, payload, priority, status, fire_key, created_at, next_run_at, max_retries, retry_count)
			VALUES ($1, json_build_object('workspace_id', $2::text, 'playbook_id', $3::text), 1, 'pending', $4,
			        LOCALTIMESTAMP, LOCALTIMESTAMP, 3, 0)`, JobType, ws, id, "playbook_schedule:"+id+":2026-06-15T13:00:00+00:00")
		if err := e.worker.Handle(context.Background(), e.tick(ws, id)); err != nil {
			t.Fatal(err)
		}
		if e.Count("playbook_schedules", "playbook_id = $1", id) != 0 {
			t.Errorf("%s: schedule mirror not removed", id)
		}
		if e.Count("jobs", "fire_key = $1 AND status = 'cancelled'", "playbook_schedule:"+id+":2026-06-15T13:00:00+00:00") != 1 {
			t.Errorf("%s: pending occurrence not cancelled", id)
		}
	}
	if e.Count("playbook_runs", "workspace_id = $1", ws) != 0 {
		t.Error("a disabled playbook must not get a run")
	}
}

func TestTenantIsolation(t *testing.T) {
	e := newEnv(t)
	a, b := e.ws("ta"), e.ws("tb")
	for _, ws := range []string{a, b} {
		e.audience(ws, "aud-"+ws)
		e.playbook(ws, "pb-"+ws, true, ptr("aud-"+ws), ptr(30), `[]`)
		e.Exec(`INSERT INTO playbook_schedules (playbook_id, workspace_id, enabled) VALUES ($1, $2, true)`, "pb-"+ws, ws)
		e.Exec(`INSERT INTO jobs (type, payload, priority, status, fire_key, created_at, next_run_at, max_retries, retry_count)
			VALUES ($1, json_build_object('workspace_id', $2::text, 'playbook_id', $3::text), 1, 'pending', $4,
			        LOCALTIMESTAMP, LOCALTIMESTAMP, 3, 0)`, JobType, ws, "pb-"+ws, "playbook_schedule:pb-"+ws+":2026-06-15T13:00:00+00:00")
	}
	if err := e.worker.Handle(context.Background(), e.tick(a, "pb-"+a)); err != nil {
		t.Fatal(err)
	}
	if e.Count("playbook_runs", "workspace_id = $1", b) != 0 || e.Count("playbook_schedules", "playbook_id = $1", "pb-"+b) != 1 ||
		e.Count("jobs", "fire_key = $1 AND status = 'pending'", "playbook_schedule:pb-"+b+":2026-06-15T13:00:00+00:00") != 1 {
		t.Error("a tick for tenant A touched tenant B")
	}

	// A job of tenant A that names tenant B's playbook is a missing playbook
	// under RLS. Python would then delete B's (non-RLS) schedule mirror and
	// cancel B's pending occurrences; Go leaves another tenant's rows alone.
	if err := e.worker.Handle(context.Background(), e.tick(a, "pb-"+b)); err != nil {
		t.Fatal(err)
	}
	if e.Count("playbook_runs", "workspace_id = $1", b) != 0 {
		t.Error("tenant B's playbook was driven by tenant A's job")
	}
	if e.Count("playbook_schedules", "playbook_id = $1", "pb-"+b) != 1 {
		t.Error("tenant A's job deleted tenant B's schedule mirror")
	}
	if e.Count("jobs", "fire_key = $1 AND status = 'pending'", "playbook_schedule:pb-"+b+":2026-06-15T13:00:00+00:00") != 1 {
		t.Error("tenant A's job cancelled tenant B's pending occurrence")
	}
	// ... and a LIKE wildcard does not reach across tenants either: ticking
	// "pb_x" would match tenant B's "pbXx" occurrence if the pattern were not
	// scoped to the job's workspace.
	e.playbook(a, "pb_x-"+a, true, ptr("aud-"+a), ptr(30), `[]`)
	wild := "playbook_schedule:pbXx-" + a + ":2026-06-15T13:00:00+00:00"
	e.Exec(`INSERT INTO jobs (type, payload, priority, status, fire_key, created_at, next_run_at, max_retries, retry_count)
		VALUES ($1, json_build_object('workspace_id', $2::text, 'playbook_id', 'pbXx'), 1, 'pending', $3,
		        LOCALTIMESTAMP, LOCALTIMESTAMP, 3, 0)`, JobType, b, wild)
	if err := e.worker.Handle(context.Background(), e.tick(a, "pb_x-"+a)); err != nil {
		t.Fatal(err)
	}
	if e.Count("jobs", "fire_key = $1 AND status = 'pending'", wild) != 1 {
		t.Error("a LIKE wildcard cancelled another tenant's occurrence")
	}

	// Outside a tenant transaction the runtime role sees no playbooks at all.
	var visible int
	if err := e.App.QueryRow(context.Background(), `SELECT count(*) FROM research_playbooks`).Scan(&visible); err != nil || visible != 0 {
		t.Errorf("runtime role sees %d playbooks outside a tenant transaction (%v)", visible, err)
	}
}

func TestLeaseLostWritesNothing(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("lease")
	e.audience(ws, "aud-"+ws)
	e.playbook(ws, "pb-"+ws, true, ptr("aud-"+ws), ptr(30), `[]`)
	job := e.tick(ws, "pb-"+ws)
	e.Cancel(job)

	err := e.worker.Handle(context.Background(), job)
	if !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("a cancelled attempt must fail with ErrLeaseLost, got %v", err)
	}
	if e.Count("playbook_runs", "workspace_id = $1", ws) != 0 || e.Count("playbook_schedules", "workspace_id = $1", ws) != 0 ||
		e.Count("jobs", "type = $1 AND workspace_id = $2", runJobType, ws) != 0 ||
		e.Count("jobs", "type = $1 AND fire_key LIKE $2", JobType, "playbook_schedule:pb-"+ws+":%") != 0 {
		t.Error("a cancelled attempt wrote domain state")
	}
	if e.Scalar(`SELECT next_run_at IS NULL FROM research_playbooks WHERE id = $1`, "pb-"+ws) != "true" {
		t.Error("a cancelled attempt wrote the playbook")
	}
}

// A failure anywhere in the tick leaves no partial state: Python commits the
// run before enqueueing its job, so a failure between them strands a pending
// run; Go does the whole tick in one transaction.
func TestFailureRollsTheWholeTickBack(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("fail")
	e.audience(ws, "aud-"+ws)
	e.playbook(ws, "pb-"+ws, true, ptr("aud-"+ws), ptr(30), `[]`)
	fn := "pbs_boom_" + strings.ReplaceAll(ws, "-", "_")
	e.Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.status = 'pending' AND NEW.type = 'research_playbook_schedule' AND NEW.payload->>'workspace_id' = %s THEN RAISE EXCEPTION 'boom'; END IF;
		RETURN NEW; END $$`, fn, jobtest.Quote(ws)))
	e.Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON jobs FOR EACH ROW EXECUTE FUNCTION %s()`, fn, fn))
	t.Cleanup(func() {
		_, _ = e.Owner.Exec(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON jobs", fn))
		_, _ = e.Owner.Exec(context.Background(), fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", fn))
	})

	err := e.worker.Handle(context.Background(), e.tick(ws, "pb-"+ws))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the insert failure, got %v", err)
	}
	if e.Count("playbook_runs", "workspace_id = $1", ws) != 0 || e.Count("jobs", "type = $1 AND workspace_id = $2", runJobType, ws) != 0 ||
		e.Count("playbook_schedules", "workspace_id = $1", ws) != 0 {
		t.Error("a failed tick left partial state behind")
	}
}

func TestInvalidPayloads(t *testing.T) {
	e := newEnv(t)
	for payload, want := range map[string]string{
		`{"workspace_id": "w"}`:                   "playbook schedule requires workspace_id and playbook_id",
		`{"playbook_id": "p"}`:                    "playbook schedule requires workspace_id and playbook_id",
		`{"workspace_id": 0, "playbook_id": "p"}`: "playbook schedule requires workspace_id and playbook_id",
		`[]`:     "'list' object has no attribute 'get'",
		`null`:   "'NoneType' object has no attribute 'get'",
		`"text"`: "'str' object has no attribute 'get'",
	} {
		job := queue.Job{ID: 1, Type: JobType, Payload: []byte(payload)}
		if err := e.worker.Handle(context.Background(), job); err == nil || err.Error() != want {
			t.Errorf("payload %s: error = %v, want %q", payload, err, want)
		}
	}
}

func TestRoutingControlsWhoClaimsTheTick(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("route")
	e.audience(ws, "aud-"+ws)
	e.playbook(ws, "pb-"+ws, true, ptr("aud-"+ws), ptr(30), `[]`)
	e.RoutingScenario(JobType, func() int64 {
		return e.Queued(JobType, ws, map[string]string{"workspace_id": ws, "playbook_id": "pb-" + ws})
	}, func(int64) bool {
		return e.Count("playbook_runs", "workspace_id = $1", ws) == 1
	})
}

func TestJobTimeoutMatchesPython(t *testing.T) {
	if got := queue.JobTimeouts[JobType]; got != 300*time.Second {
		t.Errorf("timeout = %v, Python's JOB_TIMEOUTS has 300 s", got)
	}
}
