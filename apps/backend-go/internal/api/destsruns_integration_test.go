//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destvalidate"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// Plan 10 Task 19: sync, runs, health, deliveries, retry and cancel routes against the recorded Python fixtures
// (testdata/contract/destinations) plus the store/queue properties the fixtures cannot show.

const drBase = "/api/audience-destinations"

// newDrEnv is newAudEnv with the destination routes registered. vars feeds Deps.Getenv (the executor cohort) and, as
// GO_DEST_TYPES (comma list), the destination types the Go executor may take.
func newDrEnv(t *testing.T, vars map[string]string) audEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 16)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	var types []string
	for _, s := range strings.Split(vars["GO_DEST_TYPES"], ",") {
		if s = strings.TrimSpace(s); s != "" {
			types = append(types, s)
		}
	}
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{},
		Getenv:         func(k string) string { return vars[k] },
		Flags:          config.Flags{PGLeadStore: true, Audiences: config.AudienceSettings{GoDestRoutesEnabled: true, GoDestTypes: types}},
		AudienceEvents: nopEmitter{}}
	RegisterDestinations(r, d)
	e := audEnv{mcpEnv: mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}, r: r}
	e.seed(t, "alice", pythonHash, true, "admin", true)
	e.seed(t, "bob", pythonHash, true, "user", false)
	e.seed(t, "erin", pythonHash, true, "user", false)
	alice, bob, erin := e.mustUserByName(t, "alice"), e.mustUserByName(t, "bob"), e.mustUserByName(t, "erin")
	e.workspace(t, "ws-alpha", alice.ID)
	e.workspace(t, "ws-beta", bob.ID)
	e.member(t, "ws-alpha", alice.ID, "owner")
	e.member(t, "ws-alpha", bob.ID, "viewer")
	e.member(t, "ws-alpha", erin.ID, "editor")
	e.member(t, "ws-beta", bob.ID, "owner")
	audPurge(t)
	drPurgeJobs(t)
	t.Cleanup(func() { audPurge(t); drPurgeJobs(t) })
	audLoadSeed(t)
	return e
}

func drPurgeJobs(t *testing.T) {
	t.Helper()
	if _, err := audOwner(t).Exec(context.Background(), `DELETE FROM jobs WHERE type = 'audience_destination_sync'`); err != nil {
		t.Fatal(err)
	}
}

// drReplay replays one recorded request. ids maps recorded generated ids to this run's; created ids and the clock-bound
// timestamps of freshly created runs are ignored (a seeded run's timestamps are compared exactly).
func (e audEnv) drReplay(t *testing.T, name string, ids map[string]string, extra ...contract.Option) contract.Resp {
	t.Helper()
	fx := contract.Load(t, "destinations/"+name)
	user, ws := audFixtureActor(name)
	if strings.Contains(name, "beta_own") {
		user, ws = "bob", "ws-beta"
	}
	path := audIDRe.ReplaceAllStringFunc(fx.Request.Path, func(s string) string {
		if v, ok := ids[s]; ok {
			return v
		}
		return s
	})
	_, got := e.call(user, ws, fx.Request.Method, path, "")
	opts := extra
	t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response, opts...) })
	return got
}

func drFixtureNames(t *testing.T, prefixes ...string) []string {
	t.Helper()
	ents, err := os.ReadDir("../../testdata/contract/destinations")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ent := range ents {
		n := strings.TrimSuffix(ent.Name(), ".json")
		for _, p := range prefixes {
			if strings.HasPrefix(n, p) {
				out = append(out, n)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

func (e audEnv) drJSON(t *testing.T, who, ws, method, path string) (int, map[string]any, string) {
	t.Helper()
	rec, _ := e.call(who, ws, method, path, "")
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m, rec.Body.String()
}

func drStr(t *testing.T, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := audOwner(t).QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func drJobs(t *testing.T, runID string) (n int, executor string) {
	t.Helper()
	n = audOwnerInt(t, `SELECT count(*) FROM jobs WHERE type = 'audience_destination_sync' AND fire_key = $1`, "destination_sync:"+runID)
	if n > 0 {
		executor = drStr(t, `SELECT executor FROM jobs WHERE type = 'audience_destination_sync' AND fire_key = $1 LIMIT 1`, "destination_sync:"+runID)
	}
	return
}

func (e audEnv) drAudit(t *testing.T, path string) (status int, meta map[string]any) {
	t.Helper()
	var raw string
	if err := audOwner(t).QueryRow(context.Background(), `SELECT response_status, metadata_json::text FROM governance_audit_events
		WHERE workspace_id = 'ws-alpha' AND resource_path = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, path).Scan(&status, &raw); err != nil {
		t.Fatalf("no audit event for %s: %v", path, err)
	}
	_ = json.Unmarshal([]byte(raw), &meta)
	return
}

// The read fixtures over the untouched seed.
func TestRunsCursorAndOffsetMatchFixtures(t *testing.T) {
	e := newDrEnv(t, nil)
	for _, n := range drFixtureNames(t, "runs_") {
		if n != "runs_after_changes" && n != "runs_beta_own" {
			e.drReplay(t, n, nil)
		}
	}
}

func TestHealthMatchesFixtures(t *testing.T) {
	e := newDrEnv(t, nil)
	for _, n := range drFixtureNames(t, "dest_health_") {
		if n == "dest_health_beta_own" {
			continue // recorded after sync_beta_own created a run (TestRunMutationsMatchFixtures)
		}
		e.drReplay(t, n, nil)
	}
}

func TestRunsBadCursor422(t *testing.T) {
	e := newDrEnv(t, nil)
	enc := func(s string) string {
		return destvalidate.EncodeHistoryCursor(time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC), s)
	}
	for _, c := range []string{"not-base64!!", "_w", "WzEsMl0", enc("x") + "AAAA@@@@", strings.Repeat("A", 1025)} {
		code, m, raw := e.drJSON(t, "alice", "ws-alpha", "GET", drBase+"/dst-act-webhook/runs?cursor="+c)
		if code != 422 {
			t.Errorf("cursor %q: %d %s", c, code, raw)
		}
		if len(c) <= 1024 && m["detail"] != "invalid destination run cursor" {
			t.Errorf("cursor %q detail %v", c, m["detail"])
		}
	}
	// the destination is looked up before the cursor is decoded: 404 wins
	if code, _, _ := e.drJSON(t, "alice", "ws-alpha", "GET", drBase+"/nope/runs?cursor=not-base64!!"); code != 404 {
		t.Errorf("unknown destination + bad cursor: %d", code)
	}
}

// next_cursor is the tamper-safe encoding of the last returned run: following it yields the next page, no overlap.
func TestRunsNextCursorRoundTrip(t *testing.T) {
	e := newDrEnv(t, nil)
	var seen []string
	path := drBase + "/dst-act-webhook/runs?limit=2"
	for i := 0; i < 10; i++ {
		code, m, raw := e.drJSON(t, "alice", "ws-alpha", "GET", path)
		if code != 200 {
			t.Fatal(code, raw)
		}
		for _, r := range m["runs"].([]any) {
			seen = append(seen, r.(map[string]any)["id"].(string))
		}
		nc, _ := m["next_cursor"].(string)
		if nc == "" {
			if m["has_more"] != false {
				t.Fatalf("has_more without cursor: %s", raw)
			}
			break
		}
		if i > 0 && m["offset"] != nil {
			t.Fatalf("offset with cursor: %s", raw)
		}
		path = drBase + "/dst-act-webhook/runs?limit=2&cursor=" + nc
	}
	want := []string{"run-act-w6", "run-act-w5", "run-act-w4", "run-act-w3", "run-act-w2", "run-act-w1"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("paged %v want %v", seen, want)
	}
}

func TestDeliveriesFiltersAndBounds(t *testing.T) {
	e := newDrEnv(t, nil)
	for _, n := range drFixtureNames(t, "deliv_") {
		if n != "deliv_after_cancel_pending" && n != "deliv_after_destination_delete_404" {
			e.drReplay(t, n, nil)
		}
	}
	// 422 order: unknown run 404 first, then status, then limit.
	for _, c := range []struct {
		q    string
		code int
	}{{"?limit=500", 200}, {"?limit=501", 422}, {"?limit=-1", 422}, {"?status=failed&limit=1", 200}, {"?status=nope", 422}, {"?after_id=3&status=success", 200}} {
		if code, _, raw := e.drJSON(t, "alice", "ws-alpha", "GET", drBase+"/runs/run-act-w5/deliveries"+c.q); code != c.code {
			t.Errorf("%s: %d %s", c.q, code, raw)
		}
	}
	if code, m, _ := e.drJSON(t, "alice", "ws-alpha", "GET", drBase+"/runs/nope/deliveries?status=bogus"); code != 404 || m["detail"] != "Destination run not found" {
		t.Errorf("unknown run: %d %v", code, m)
	}
}

func TestSyncCreatesRunAndJob(t *testing.T) {
	// cohort + GO_DEST_TYPES: webhook goes to Go, hubspot (not listed) and an out-of-cohort workspace stay on Python
	vars := map[string]string{"GO_EXECUTOR_ENABLED": "true", "GO_EXECUTOR_WORKSPACES": "ws-alpha", "GO_DEST_TYPES": "webhook,airtable"}
	e := newDrEnv(t, vars)
	for _, c := range []struct{ dest, executor string }{{"dst-act-airtable", "go"}, {"dst-act-instantly", "python"}} {
		code, m, raw := e.drJSON(t, "alice", "ws-alpha", "POST", drBase+"/"+c.dest+"/sync")
		if code != 202 {
			t.Fatal(code, raw)
		}
		id, _ := m["id"].(string)
		if m["status"] != "pending" || m["destination_id"] != c.dest || len(id) != 36 {
			t.Fatalf("%s: %s", c.dest, raw)
		}
		if n, ex := drJobs(t, id); n != 1 || ex != c.executor {
			t.Errorf("%s: jobs=%d executor=%q want 1/%s", c.dest, n, ex, c.executor)
		}
		alice := e.mustUserByName(t, "alice")
		if got := drStr(t, `SELECT requested_by FROM destination_runs WHERE id = $1`, id); got != fmt.Sprint(alice.ID) {
			t.Errorf("requested_by %q want %v", got, alice.ID)
		}
		if got := drStr(t, `SELECT payload::text FROM jobs WHERE fire_key = $1`, "destination_sync:"+id); !strings.Contains(got, `"run_id"`) || !strings.Contains(got, `"workspace_id"`) {
			t.Errorf("payload %s", got)
		}
	}
	// the same type with the workspace out of the cohort stays on Python (Getenv reads the map per call)
	vars["GO_EXECUTOR_WORKSPACES"] = "ws-other"
	_, m, _ := e.drJSON(t, "alice", "ws-alpha", "POST", drBase+"/dst-act-webhook/sync")
	if n, ex := drJobs(t, m["id"].(string)); n != 1 || ex != "python" {
		t.Errorf("out of cohort: %d %q", n, ex)
	}
}

func TestSyncReturnsExistingActiveRun(t *testing.T) {
	e := newDrEnv(t, nil)
	for dest, run := range map[string]string{"dst-act-hubspot": "run-act-hub2", "dst-act-salesforce": "run-act-sf1", "dst-act-meta_ads": "run-act-meta1"} {
		code, m, raw := e.drJSON(t, "alice", "ws-alpha", "POST", drBase+"/"+dest+"/sync")
		if code != 202 || m["id"] != run {
			t.Fatalf("%s: %d %s", dest, code, raw)
		}
		if n, _ := drJobs(t, run); n != 0 {
			t.Errorf("%s: existing run got a job", dest)
		}
	}
	if n := audOwnerInt(t, `SELECT count(*) FROM destination_runs WHERE destination_id = 'dst-act-hubspot'`); n != 2 {
		t.Errorf("runs %d", n)
	}
}

func TestSyncConcurrentSingleRun(t *testing.T) {
	e := newDrEnv(t, nil)
	var mu sync.Mutex
	ids := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, _ := e.call("alice", "ws-alpha", "POST", drBase+"/dst-act-airtable/sync", "")
			var m map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &m)
			mu.Lock()
			defer mu.Unlock()
			if rec.Code != 202 {
				t.Errorf("status %d %s", rec.Code, rec.Body.String())
				return
			}
			ids[m["id"].(string)]++
		}()
	}
	wg.Wait()
	if len(ids) != 1 {
		t.Fatalf("distinct runs %v", ids)
	}
	if n := audOwnerInt(t, `SELECT count(*) FROM destination_runs WHERE destination_id = 'dst-act-airtable'`); n != 1 {
		t.Errorf("run rows %d", n)
	}
	if n := audOwnerInt(t, `SELECT count(*) FROM jobs WHERE type = 'audience_destination_sync'`); n != 1 {
		t.Errorf("jobs %d", n)
	}
}

func TestSyncDisabled409(t *testing.T) {
	e := newDrEnv(t, nil)
	code, m, _ := e.drJSON(t, "alice", "ws-alpha", "POST", drBase+"/dst-act-webhook-off/sync")
	if code != 409 || m["detail"] != "Destination is disabled" {
		t.Fatalf("%d %v", code, m)
	}
	if n := audOwnerInt(t, `SELECT count(*) FROM destination_runs WHERE destination_id = 'dst-act-webhook-off'`); n != 2 {
		t.Errorf("a refused sync created a run: %d", n)
	}
}

func TestRetryTransitionsAndAudit(t *testing.T) {
	e := newDrEnv(t, map[string]string{"GO_EXECUTOR_ENABLED": "true", "GO_EXECUTOR_WORKSPACES": "ws-alpha", "GO_DEST_TYPES": "webhook"})
	code, m, raw := e.drJSON(t, "alice", "ws-alpha", "POST", drBase+"/runs/run-act-w6/retry")
	if code != 202 || m["status"] != "pending" || m["error"] != nil || m["finished_at"] != nil || m["failed"] != float64(3) {
		t.Fatalf("%d %s", code, raw)
	}
	if n, ex := drJobs(t, "run-act-w6"); n != 1 || ex != "go" {
		t.Errorf("job %d %q", n, ex)
	}
	st, meta := e.drAudit(t, drBase+"/runs/run-act-w6/retry")
	want := map[string]any{"action": "audience_destination.run.retry", "destination_id": "dst-act-webhook", "run_id": "run-act-w6", "previous_failed": float64(3)}
	if st != 202 || fmt.Sprint(meta) != fmt.Sprint(want) {
		t.Errorf("audit %d %v", st, meta)
	}
	// the retried run is active again: a second retry and a sync both see it
	if code, _, _ := e.drJSON(t, "alice", "ws-alpha", "POST", drBase+"/runs/run-act-w6/retry"); code != 409 {
		t.Errorf("second retry %d", code)
	}
	if _, m, _ := e.drJSON(t, "alice", "ws-alpha", "POST", drBase+"/dst-act-webhook/sync"); m["id"] != "run-act-w6" {
		t.Errorf("sync after retry %v", m)
	}
}

func TestRetryWrongStatus409(t *testing.T) {
	e := newDrEnv(t, nil)
	for run, detail := range map[string]string{
		"run-act-w1": "Only failed or completed-with-errors runs can be retried", "run-act-w4": "Only failed or completed-with-errors runs can be retried",
		"run-act-hub2": "Only failed or completed-with-errors runs can be retried", "run-act-sf1": "Only failed or completed-with-errors runs can be retried",
		"run-act-meta1": "Only failed or completed-with-errors runs can be retried", "run-act-off1": "Destination is disabled",
	} {
		code, m, raw := e.drJSON(t, "alice", "ws-alpha", "POST", drBase+"/runs/"+run+"/retry")
		if code != 409 || m["detail"] != detail {
			t.Errorf("%s: %d %s", run, code, raw)
		}
		if n, _ := drJobs(t, run); n != 0 {
			t.Errorf("%s: refused retry enqueued", run)
		}
	}
	// completed_with_errors is retryable
	if _, err := audOwner(t).Exec(context.Background(), `UPDATE destination_runs SET status = 'completed_with_errors' WHERE id = 'run-act-w5'`); err != nil {
		t.Fatal(err)
	}
	if code, _, raw := e.drJSON(t, "alice", "ws-alpha", "POST", drBase+"/runs/run-act-w5/retry"); code != 202 {
		t.Errorf("completed_with_errors: %d %s", code, raw)
	}
}

func TestCancelPendingFinishesCancelled(t *testing.T) {
	e := newDrEnv(t, nil)
	if _, err := audOwner(t).Exec(context.Background(), `INSERT INTO jobs (type, payload, status, fire_key, workspace_id, executor, next_run_at)
		VALUES ('audience_destination_sync', '{"workspace_id":"ws-alpha","run_id":"run-act-sf1"}', 'pending', 'destination_sync:run-act-sf1', 'ws-alpha', 'python', now())`); err != nil {
		t.Fatal(err)
	}
	code, m, raw := e.drJSON(t, "alice", "ws-alpha", "POST", drBase+"/runs/run-act-sf1/cancel")
	if code != 202 || m["status"] != "cancelled" || m["finished_at"] == nil {
		t.Fatalf("%d %s", code, raw)
	}
	if n := audOwnerInt(t, `SELECT count(*) FROM destination_deliveries WHERE run_id = 'run-act-sf1' AND status <> 'cancelled'`); n != 0 {
		t.Errorf("%d deliveries not cancelled", n)
	}
	// Python parity: the queued job is left for the engine, which finds the run terminal
	if n, _ := drJobs(t, "run-act-sf1"); n != 1 {
		t.Errorf("job count %d", n)
	}
	st, meta := e.drAudit(t, drBase+"/runs/run-act-sf1/cancel")
	want := map[string]any{"action": "audience_destination.run.cancel", "destination_id": "dst-act-salesforce", "run_id": "run-act-sf1", "previous_status": "pending"}
	if st != 202 || fmt.Sprint(meta) != fmt.Sprint(want) {
		t.Errorf("audit %d %v", st, meta)
	}
}

func TestCancelRunningRequestsCancellation(t *testing.T) {
	e := newDrEnv(t, nil)
	code, m, raw := e.drJSON(t, "alice", "ws-alpha", "POST", drBase+"/runs/run-act-hub2/cancel")
	if code != 202 || m["status"] != "cancelling" || m["error"] != "Cancellation requested" || m["finished_at"] != nil {
		t.Fatalf("%d %s", code, raw)
	}
	if _, meta := e.drAudit(t, drBase+"/runs/run-act-hub2/cancel"); meta["previous_status"] != "running" {
		t.Errorf("audit %v", meta)
	}
	// cancelling again is allowed and stays cancelling
	code, m, raw = e.drJSON(t, "alice", "ws-alpha", "POST", drBase+"/runs/run-act-hub2/cancel")
	if code != 202 || m["status"] != "cancelling" {
		t.Fatalf("again: %d %s", code, raw)
	}
	if _, meta := e.drAudit(t, drBase+"/runs/run-act-hub2/cancel"); meta["previous_status"] != "cancelling" {
		t.Errorf("audit %v", meta)
	}
}

func TestCancelTerminal409(t *testing.T) {
	e := newDrEnv(t, nil)
	for _, run := range []string{"run-act-w1", "run-act-w4", "run-act-off1", "run-act-w5"} {
		code, m, _ := e.drJSON(t, "alice", "ws-alpha", "POST", drBase+"/runs/"+run+"/cancel")
		if code != 409 || m["detail"] != "Only pending or running destination runs can be cancelled" {
			t.Errorf("%s: %d %v", run, code, m)
		}
	}
	if st, meta := e.drAudit(t, drBase+"/runs/run-act-w1/cancel"); st != 409 || len(meta) != 0 {
		t.Errorf("refused cancel audit %d %v", st, meta)
	}
}

func TestRoutePrecedenceStaticBeforeParam(t *testing.T) {
	e := newDrEnv(t, nil)
	// destinations whose ids collide with the static path segments
	for _, id := range []string{"runs", "inbound"} {
		if _, err := audOwner(t).Exec(context.Background(), `INSERT INTO audience_destinations (id, workspace_id, audience_id, name, destination_type, enabled, config, field_map)
			SELECT $1, workspace_id, id, $1, 'webhook', true, '{}'::json, '{}'::json FROM audiences WHERE workspace_id = 'ws-alpha' ORDER BY id LIMIT 1`, id); err != nil {
			t.Fatal(err)
		}
	}
	// /runs/{run_id}/deliveries is the static route; /{destination_id}/runs is the parametrized one
	if code, m, raw := e.drJSON(t, "alice", "ws-alpha", "GET", drBase+"/runs/runs"); code != 200 || m["runs"] == nil {
		t.Errorf("destination 'runs' list: %d %s", code, raw)
	}
	if code, _, raw := e.drJSON(t, "alice", "ws-alpha", "GET", drBase+"/runs/run-act-w5/deliveries"); code != 200 || !strings.HasPrefix(raw, "[") {
		t.Errorf("deliveries: %d %.80s", code, raw)
	}
	if code, m, _ := e.drJSON(t, "alice", "ws-alpha", "POST", drBase+"/runs/run-act-w6/retry"); code != 202 || m["id"] != "run-act-w6" {
		t.Errorf("retry: %d", code)
	}
	if code, m, raw := e.drJSON(t, "alice", "ws-alpha", "GET", drBase+"/inbound/health"); code != 200 || m["destination_id"] != "inbound" {
		t.Errorf("destination 'inbound' health: %d %s", code, raw)
	}
	if code, _, _ := e.drJSON(t, "alice", "ws-alpha", "POST", drBase+"/runs/sync"); code != 202 {
		t.Errorf("destination 'runs' sync: %d", code)
	}
}

func TestRolesAndTenantIsolation(t *testing.T) {
	e := newDrEnv(t, nil)
	writes := []string{drBase + "/dst-act-webhook/sync", drBase + "/runs/run-act-w6/retry", drBase + "/runs/run-act-hub2/cancel"}
	for _, p := range writes {
		if code, m, _ := e.drJSON(t, "bob", "ws-alpha", "POST", p); code != 403 || m["detail"] != "Workspace permission denied: activation.write" {
			t.Errorf("viewer %s: %d %v", p, code, m)
		}
		if code, _, _ := e.drJSON(t, "", "", "POST", p); code != 401 {
			t.Errorf("anon %s: %d", p, code)
		}
	}
	for _, p := range []string{drBase + "/dst-act-webhook/runs", drBase + "/dst-act-webhook/health", drBase + "/runs/run-act-w5/deliveries"} {
		if code, _, raw := e.drJSON(t, "bob", "ws-alpha", "GET", p); code != 200 {
			t.Errorf("viewer read %s: %d %s", p, code, raw)
		}
	}
	// ws-beta's destination and run are invisible from ws-alpha, in every route
	for _, c := range []struct{ method, path, detail string }{
		{"POST", drBase + "/dst-beta-crm/sync", "Destination not found"}, {"GET", drBase + "/dst-beta-crm/runs", "Destination not found"},
		{"GET", drBase + "/dst-beta-crm/health", "Destination not found"}, {"GET", drBase + "/runs/run-beta-crm1/deliveries", "Destination run not found"},
		{"POST", drBase + "/runs/run-beta-crm1/retry", "Destination run not found"}, {"POST", drBase + "/runs/run-beta-crm1/cancel", "Destination run not found"},
	} {
		if code, m, _ := e.drJSON(t, "alice", "ws-alpha", c.method, c.path); code != 404 || m["detail"] != c.detail {
			t.Errorf("%s %s: %d %v", c.method, c.path, code, m)
		}
	}
	// and nothing changed
	if n := audOwnerInt(t, `SELECT count(*) FROM destination_runs WHERE destination_id = 'dst-beta-crm'`); n != 1 {
		t.Errorf("foreign runs %d", n)
	}
}

// The recorded mutation chain, in recording order: later fixtures see the state the earlier ones left. Runs created by the
// recording have generated ids and clock-bound timestamps; seeded runs are compared exactly (finished_at of a run the
// request itself finishes is clock-bound too).
func TestRunMutationsMatchFixtures(t *testing.T) {
	e := newDrEnv(t, nil)
	ids := map[string]string{}
	exact := func(names ...string) {
		for _, n := range names {
			e.drReplay(t, n, ids)
		}
	}
	fresh := []contract.Option{contract.IgnorePaths("$.id"), contract.IgnoreShapeOf("$.created_at")}
	exact("sync_no_auth", "sync_viewer_403", "sync_unknown_404", "sync_foreign_404", "sync_disabled_409",
		"sync_existing_seeded_pending", "sync_existing_seeded_running", "sync_existing_seeded_cancelling",
		"retry_no_auth", "retry_viewer_403", "retry_unknown_404", "retry_foreign_404", "retry_completed_409", "retry_cancelled_409",
		"retry_running_409", "retry_disabled_destination_409",
		"retry_failed", "retry_pending_now_409", "retry_by_erin", "retry_failed_second_destination")
	e.drReplay(t, "sync_by_erin", ids, fresh...)
	e.drReplay(t, "sync_beta_own", ids, fresh...)
	// recorded after it: the newest run of dst-beta-crm is the one the sync created
	e.drReplay(t, "runs_beta_own", ids, contract.IgnorePaths("$.runs[0].id"), contract.IgnoreShapeOf("$.runs[0].created_at"))
	e.drReplay(t, "dest_health_beta_own", ids, contract.IgnorePaths("$.latest_run.id"), contract.IgnoreShapeOf("$.latest_run.created_at"))
	e.drReplay(t, "sync_after_terminal", ids, fresh...)
	got := e.drReplay(t, "sync_new", ids, fresh...)
	var m map[string]any
	_ = json.Unmarshal(got.Body, &m)
	ids["00000000-0000-4000-8000-000000002132"], _ = m["id"].(string)
	e.drReplay(t, "sync_existing_active", ids, fresh...)
	exact("cancel_no_auth", "cancel_viewer_403", "cancel_unknown_404", "cancel_foreign_404", "cancel_completed_409", "cancel_cancelled_409",
		"cancel_failed_409", "cancel_running", "cancel_cancelling_again")
	clock := contract.IgnoreShapeOf("$.finished_at")
	e.drReplay(t, "cancel_pending", ids, clock)
	e.drReplay(t, "cancel_retried_pending", ids, clock)
	e.drReplay(t, "cancel_new_sync_run", ids, append([]contract.Option{clock}, fresh...)...)
	e.drReplay(t, "deliv_after_cancel_pending", ids)
	e.drReplay(t, "runs_after_changes", ids)
	e.drReplay(t, "health_after_changes", ids)
	// the cascade of a destination delete takes its runs with it
	if _, err := audOwner(t).Exec(context.Background(), `DELETE FROM audience_destinations WHERE id = 'dst-act-salesforce'`); err != nil {
		t.Fatal(err)
	}
	e.drReplay(t, "deliv_after_destination_delete_404", ids)
}
