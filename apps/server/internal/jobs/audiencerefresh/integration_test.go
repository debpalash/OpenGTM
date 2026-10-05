package audiencerefresh

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

var frozen = time.Date(2026, 6, 15, 12, 0, 0, 250000000, time.UTC)

type env struct {
	*jobtest.Env
	worker *Worker
	spaces []string
}

func newEnv(t *testing.T, opts Options) *env {
	t.Helper()
	e := &env{Env: jobtest.New(t)}
	e.worker = NewWorker(e.App, nil, opts)
	e.worker.Now = func() time.Time { return frozen }
	t.Cleanup(e.cleanup)
	return e
}

func (e *env) ws(label string) string {
	ws := jobtest.Unique("ar-" + label)
	e.spaces = append(e.spaces, ws)
	return ws
}

func (e *env) cleanup() {
	ctx := context.Background()
	for _, ws := range e.spaces {
		for _, t := range []string{"audience_membership_events", "audience_members", "destination_runs", "audience_destinations",
			"workbook_rows", "workbooks", "triggers", "leads", "audience_schedules", "audiences"} {
			_, _ = e.Owner.Exec(ctx, "DELETE FROM "+t+" WHERE workspace_id = $1", ws)
		}
		_, _ = e.Owner.Exec(ctx, `DELETE FROM jobs WHERE type IN ($1, 'trigger_eval', 'audience_destination_sync')
			AND (workspace_id = $2 OR payload->>'workspace_id' = $2)`, JobType, ws)
	}
}

// leads seeds n leads with distinct scores into ws, ids unspecified (serial).
func (e *env) leads(ws string, n int, city string) {
	e.Exec(`INSERT INTO leads (workspace_id, company, city, score, score_tier, status, source, collection_job_id, created_at, updated_at)
		SELECT $1, 'Co ' || i, $3, 1000000 - i, 'hot', 'new', 'csv', '', '2026-01-01T00:00:00+00:00', '2026-01-01T00:00:00+00:00'
		FROM generate_series(1, $2::int) AS i`, ws, n, city)
}

func (e *env) audience(ws, id, filters string, enabled bool, minutes int) {
	e.Exec(`INSERT INTO audiences (id, workspace_id, name, filters, refresh_enabled, refresh_interval_minutes)
		VALUES ($1::text, $2, $1::text, $3::json, $4, $5)`, id, ws, filters, enabled, minutes)
}

func (e *env) tick(ws, audience string) queue.Job {
	return e.Claimed(JobType, "", map[string]string{"workspace_id": ws, "audience_id": audience})
}

func (e *env) members(ws, audience string) int {
	return e.Count("audience_members", "workspace_id = $1 AND audience_id = $2", ws, audience)
}

func TestRefreshMaterializesTheAudienceInPages(t *testing.T) {
	e := newEnv(t, Options{}) // the production page size (500)
	ws := e.ws("big")
	e.leads(ws, 1234, "Pune")
	e.leads(ws, 20, "Goa")
	e.audience(ws, "aud", `{"city": "Pune"}`, true, 30)

	if err := e.worker.Handle(context.Background(), e.tick(ws, "aud")); err != nil {
		t.Fatal(err)
	}
	if n := e.members(ws, "aud"); n != 1234 {
		t.Fatalf("members = %d, want 1234", n)
	}
	if n := e.Count("audience_membership_events", "workspace_id = $1 AND event_type = 'entered'", ws); n != 1234 {
		t.Errorf("entered events = %d", n)
	}
	if e.Scalar(`SELECT member_count FROM audiences WHERE id = 'aud' AND workspace_id = $1`, ws) != "1234" ||
		e.Scalar(`SELECT refresh_health FROM audiences WHERE id = 'aud' AND workspace_id = $1`, ws) != "healthy" {
		t.Error("audience counters/health not updated")
	}
	// Members of one refresh share a token, and a second refresh changes nothing.
	if e.Scalar(`SELECT count(DISTINCT refresh_token) FROM audience_members WHERE workspace_id = $1`, ws) != "1" {
		t.Error("members of one refresh must share one refresh token")
	}
	if err := e.worker.Handle(context.Background(), e.tick(ws, "aud")); err != nil {
		t.Fatal(err)
	}
	if e.Count("audience_membership_events", "workspace_id = $1", ws) != 1234 || e.members(ws, "aud") != 1234 {
		t.Error("an unchanged refresh must not add events or members")
	}
	// Everyone leaves: events and deletes are paged too.
	e.Exec(`UPDATE audiences SET filters = '{"city": "Nowhere"}' WHERE id = 'aud' AND workspace_id = $1`, ws)
	if err := e.worker.Handle(context.Background(), e.tick(ws, "aud")); err != nil {
		t.Fatal(err)
	}
	if e.members(ws, "aud") != 0 || e.Count("audience_membership_events", "workspace_id = $1 AND event_type = 'exited'", ws) != 1234 {
		t.Error("expected every member to exit with an event")
	}
}

func TestTenantIsolation(t *testing.T) {
	e := newEnv(t, Options{PageSize: 10})
	a, b := e.ws("ta"), e.ws("tb")
	e.leads(a, 5, "Pune")
	e.leads(b, 7, "Pune")
	e.audience(a, "aud-a", `{}`, true, 30)
	e.audience(b, "aud-b", `{}`, true, 30)
	e.Exec(`INSERT INTO audience_schedules (audience_id, workspace_id, enabled) VALUES ('aud-b', $1, true)`, b)
	occurrence := "audience_refresh:aud-b:2026-06-15T13:00:00+00:00"
	e.Exec(`INSERT INTO jobs (type, payload, priority, status, fire_key, created_at, next_run_at, max_retries, retry_count)
		VALUES ($1, json_build_object('workspace_id', $2::text, 'audience_id', 'aud-b'), 1, 'pending', $3, LOCALTIMESTAMP, LOCALTIMESTAMP, 3, 0)`,
		JobType, b, occurrence)

	if err := e.worker.Handle(context.Background(), e.tick(a, "aud-a")); err != nil {
		t.Fatal(err)
	}
	if e.members(a, "aud-a") != 5 {
		t.Errorf("tenant A's audience must see only tenant A's leads, got %d members", e.members(a, "aud-a"))
	}
	if e.members(b, "aud-b") != 0 || e.Count("audience_membership_events", "workspace_id = $1", b) != 0 {
		t.Error("tenant B was touched by a refresh for tenant A")
	}

	// A job of tenant A naming tenant B's audience finds nothing under RLS.
	// Python would then delete B's (non-RLS) schedule mirror and cancel B's
	// pending occurrence; Go leaves another tenant's rows alone.
	if err := e.worker.Handle(context.Background(), e.tick(a, "aud-b")); err != nil {
		t.Fatal(err)
	}
	if e.members(b, "aud-b") != 0 {
		t.Error("tenant B's audience was refreshed by tenant A's job")
	}
	if e.Count("audience_schedules", "audience_id = 'aud-b'") != 1 ||
		e.Count("jobs", "fire_key = $1 AND status = 'pending'", occurrence) != 1 {
		t.Error("tenant A's job removed tenant B's schedule")
	}
	// Outside a tenant transaction the runtime role sees no audiences or leads.
	var visible int
	if err := e.App.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM audiences) + (SELECT count(*) FROM leads)`).Scan(&visible); err != nil || visible != 0 {
		t.Errorf("runtime role sees %d rows outside a tenant transaction (%v)", visible, err)
	}
}

func TestLeaseLostWritesNothing(t *testing.T) {
	e := newEnv(t, Options{PageSize: 10})
	ws := e.ws("lease")
	e.leads(ws, 25, "Pune")
	e.audience(ws, "aud", `{}`, true, 30)
	e.Exec(`INSERT INTO audience_destinations (id, workspace_id, audience_id, name, destination_type, enabled, config, field_map)
		VALUES ($1::text, $2, 'aud', 'd', 'webhook', true, '{}', '{}')`, "d-"+ws, ws)

	assertUntouched := func(when string) {
		t.Helper()
		if e.members(ws, "aud") != 0 || e.Count("audience_membership_events", "workspace_id = $1", ws) != 0 ||
			e.Count("destination_runs", "workspace_id = $1", ws) != 0 || e.Count("audience_schedules", "workspace_id = $1", ws) != 0 ||
			e.Count("jobs", "type = $1 AND fire_key LIKE 'audience_refresh:aud:%'", JobType) != 0 ||
			e.Scalar(`SELECT refreshed_at IS NULL AND next_refresh_at IS NULL AND refresh_health = 'unverified'
				FROM audiences WHERE id = 'aud' AND workspace_id = $1`, ws) != "true" {
			t.Errorf("%s: a cancelled attempt wrote domain state", when)
		}
	}

	// Cancelled before the attempt wrote anything: the lease check fails at commit.
	job := e.tick(ws, "aud")
	e.Cancel(job)
	if err := e.worker.Handle(context.Background(), job); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("a cancelled attempt must fail with ErrLeaseLost, got %v", err)
	}
	assertUntouched("cancelled before the attempt")

	// Cancelled after the whole refresh was written but before the commit.
	job = e.tick(ws, "aud")
	e.worker.beforeCommit = func() { e.Cancel(job) }
	err := e.worker.Handle(context.Background(), job)
	e.worker.beforeCommit = nil
	if !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("cancelled during the attempt: got %v", err)
	}
	assertUntouched("cancelled mid-refresh")
	// A lost lease is not a refresh failure: the health is left alone.
	if e.Scalar(`SELECT consecutive_refresh_failures FROM audiences WHERE id = 'aud' AND workspace_id = $1`, ws) != "0" {
		t.Error("a lost lease was counted as a refresh failure")
	}
}

// Unlike Python, which commits the member diff before enqueueing the destination
// syncs, a failure anywhere in the refresh leaves no partial diff: the whole
// refresh rolls back, the failure is recorded on the audience and the next
// occurrence is still booked.
func TestFailureRollsTheWholeRefreshBack(t *testing.T) {
	e := newEnv(t, Options{PageSize: 10})
	ws := e.ws("fail")
	e.leads(ws, 25, "Pune")
	e.audience(ws, "aud", `{}`, true, 30)
	e.Exec(`INSERT INTO audience_destinations (id, workspace_id, audience_id, name, destination_type, enabled, config, field_map)
		VALUES ($1::text, $2, 'aud', 'd', 'webhook', true, '{}', '{}')`, "d-"+ws, ws)
	fn := "ar_boom_" + strings.ReplaceAll(ws, "-", "_")
	e.Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.workspace_id = %s THEN RAISE EXCEPTION 'boom'; END IF; RETURN NEW; END $$`, fn, jobtest.Quote(ws)))
	e.Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON destination_runs FOR EACH ROW EXECUTE FUNCTION %s()`, fn, fn))
	t.Cleanup(func() {
		_, _ = e.Owner.Exec(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON destination_runs", fn))
		_, _ = e.Owner.Exec(context.Background(), fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", fn))
	})

	err := e.worker.Handle(context.Background(), e.tick(ws, "aud"))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the failure to surface for the queue, got %v", err)
	}
	if e.members(ws, "aud") != 0 || e.Count("audience_membership_events", "workspace_id = $1", ws) != 0 {
		t.Error("a failed refresh left a partial diff behind")
	}
	if e.Scalar(`SELECT refresh_health || '/' || consecutive_refresh_failures || '/' || (last_refresh_error LIKE '%boom%')
		FROM audiences WHERE id = 'aud' AND workspace_id = $1`, ws) != "degraded/1/true" {
		t.Error("the failure must be recorded on the audience")
	}
	if e.Count("jobs", "type = $1 AND status = 'pending' AND fire_key LIKE 'audience_refresh:aud:%'", JobType) != 1 ||
		e.Count("audience_schedules", "audience_id = 'aud' AND enabled") != 1 {
		t.Error("a failed refresh must still book the next occurrence")
	}
}

func TestMembershipEventsFireAutomationsOnlyWhenEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%v", enabled), func(t *testing.T) {
			e := newEnv(t, Options{AutomationsEnabled: enabled})
			ws := e.ws("auto")
			e.leads(ws, 3, "Pune")
			e.audience(ws, "aud", `{}`, true, 30)
			e.Exec(`INSERT INTO workbooks (id, name, workspace_id) VALUES ($1::text, 'wb', $2)`, "wb-"+ws, ws)
			e.Exec(`INSERT INTO workbook_rows (workbook_id, workspace_id, lead_id, data, enrichments, position)
				SELECT $1::text, $2::text, id, '{}', '{}', 0 FROM leads WHERE workspace_id = $2::text ORDER BY id LIMIT 2`, "wb-"+ws, ws)
			e.Exec(`INSERT INTO triggers (id, workspace_id, name, enabled, trigger_type, trigger_config, actions, scope_workbook_ids)
				VALUES ($1::text, $2, 'enter', true, 'on_audience_enter', '{}', '[]', '[]')`, "t-"+ws, ws)

			if err := e.worker.Handle(context.Background(), e.tick(ws, "aud")); err != nil {
				t.Fatal(err)
			}
			want := 0
			if enabled {
				want = 2 // the third lead is in no workbook: nothing to fire
			}
			if n := e.Count("jobs", "type = 'trigger_eval' AND workspace_id = $1 AND fire_key IS NULL AND payload->>'fire_source' = 'audience_entered'", ws); n != want {
				t.Errorf("trigger_eval jobs = %d, want %d", n, want)
			}
		})
	}
}

func TestRoutingControlsWhoClaimsRefreshes(t *testing.T) {
	e := newEnv(t, Options{})
	ws := e.ws("route")
	e.leads(ws, 3, "Pune")
	e.audience(ws, "aud", `{}`, true, 30)
	e.RoutingScenario(JobType, func() int64 {
		return e.Queued(JobType, ws, map[string]string{"workspace_id": ws, "audience_id": "aud"})
	}, func(int64) bool {
		return e.members(ws, "aud") == 3
	})
}

// A failing attempt goes through the real queue: the handler records the
// failure and books the next occurrence, finalize schedules the retry, and the
// registered reconciler recognises the handler already recorded this attempt
// (a future next_refresh_at) instead of counting the failure twice.
func TestQueueRunsTheReconcilerWithoutDoubleCounting(t *testing.T) {
	e := newEnv(t, Options{})
	e.worker.Now = func() time.Time { return time.Now().UTC() }
	ws := e.ws("qfail")
	e.leads(ws, 3, "Pune")
	e.audience(ws, "aud", `{}`, true, 30)
	fn := "ar_qboom_" + strings.ReplaceAll(ws, "-", "_")
	e.Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.workspace_id = %s THEN RAISE EXCEPTION 'boom'; END IF; RETURN NEW; END $$`, fn, jobtest.Quote(ws)))
	e.Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON audience_members FOR EACH ROW EXECUTE FUNCTION %s()`, fn, fn))
	t.Cleanup(func() {
		_, _ = e.Owner.Exec(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON audience_members", fn))
		_, _ = e.Owner.Exec(context.Background(), fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", fn))
	})
	e.SetRoute(JobType, "go")
	jobID := e.Queued(JobType, ws, map[string]string{"workspace_id": ws, "audience_id": "aud"})
	e.StartQueue(JobType)
	e.Eventually("the failed attempt to be retried", func() bool {
		return strings.HasPrefix(e.Scalar(`SELECT error FROM jobs WHERE id = $1`, jobID), "Retry 1: ")
	})
	time.Sleep(300 * time.Millisecond) // let the reconciler run
	if e.Scalar(`SELECT consecutive_refresh_failures FROM audiences WHERE id = 'aud' AND workspace_id = $1`, ws) != "1" {
		t.Error("the failure was counted twice (handler and reconciler)")
	}
	if e.members(ws, "aud") != 0 {
		t.Error("the failed refresh left members behind")
	}
}

func TestInvalidPayloadsAndMissingAudiences(t *testing.T) {
	e := newEnv(t, Options{})
	for payload, want := range map[string]string{
		`[]`:   "'list' object has no attribute 'get'",
		`null`: "'NoneType' object has no attribute 'get'",
	} {
		if err := e.worker.Handle(context.Background(), queue.Job{ID: 1, Type: JobType, Payload: []byte(payload)}); err == nil || err.Error() != want {
			t.Errorf("payload %s: error = %v, want %q", payload, err, want)
		}
	}
	// Missing ids return quietly, like Python (which logs a warning).
	for _, payload := range []string{`{}`, `{"workspace_id": "w"}`, `{"audience_id": "a"}`, `{"workspace_id": "", "audience_id": "a"}`, `{"workspace_id": "w", "audience_id": 0}`} {
		if err := e.worker.Handle(context.Background(), queue.Job{ID: 1, Type: JobType, Payload: []byte(payload)}); err != nil {
			t.Errorf("payload %s: %v", payload, err)
		}
	}
	// The failure reconciler insists on both ids.
	err := e.worker.Reconcile(context.Background(), queue.Job{ID: 1, Type: JobType, Payload: []byte(`{"workspace_id": "w"}`)}, "x", true)
	if err == nil || err.Error() != "audience refresh failure payload requires workspace_id and audience_id" {
		t.Errorf("reconciler error = %v", err)
	}
}

func TestJobTimeoutMatchesPython(t *testing.T) {
	if got := queue.JobTimeouts[JobType]; got != 900*time.Second {
		t.Errorf("timeout = %v, Python's JOB_TIMEOUTS has 900 s", got)
	}
}
