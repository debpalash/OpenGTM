//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/playbooks"
)

// newPBRunEnv is the T16 world plus the run routes, the audit middleware and the recorded run and result rows
// (taken from the fixtures themselves). getenv drives the executor cohort (nil: nothing set).
func newPBRunEnv(t *testing.T, getenv func(string) string) pbEnv {
	t.Helper()
	e := newPBEnv(t).withRunRoutes(getenv)
	e.seedRuns(t)
	return e
}

// withRunRoutes serves the CRUD and run routes with the audit middleware over the same pool; getenv drives the
// executor cohort (nil: nothing set).
func (e pbEnv) withRunRoutes(getenv func(string) string) pbEnv {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(e.pool, nil))
	d := Deps{Pool: e.pool, Signer: testSigner, Identity: e.st, Getenv: getenv,
		Flags: config.Flags{PGLeadStore: true, Governance: config.GovernanceSettings{GoExecutorPlaybooks: true}}}
	registerPlaybooksRoutes(r, d)
	registerPlaybookRuns(r, d)
	e.h = r.Handler()
	return e
}

func fxBody(t *testing.T, name string) json.RawMessage {
	t.Helper()
	return contract.Load(t, "playbooks/"+name).Response.Body
}

// seedRuns replaces the generic runs of seedWorld with the recorded ones (counts, timestamps, errors), adds the
// recorded results and a pending job for each active run.
func (e pbEnv) seedRuns(t *testing.T) {
	t.Helper()
	e.exec(t, `UPDATE research_playbooks SET version = 6 WHERE id = 'pb-gov-multi'`)
	e.exec(t, `DELETE FROM playbook_runs WHERE workspace_id IN ('ws-alpha','ws-beta')`)
	type runJSON struct {
		Attempted, Succeeded, Failed, PromptVersion, MaxMembers int
		AudienceID, ID, PlaybookID, Status, CreatedAt           string
		Error, StartedAt, FinishedAt                            *string
	}
	var all []runJSON
	for _, fx := range []string{"pb_runs_multi_default", "pb_runs_sched_default", "pb_runs_beta_playbook"} {
		var page struct {
			Runs []struct {
				Attempted     int     `json:"attempted"`
				AudienceID    string  `json:"audience_id"`
				CreatedAt     string  `json:"created_at"`
				Error         *string `json:"error"`
				Failed        int     `json:"failed"`
				FinishedAt    *string `json:"finished_at"`
				ID            string  `json:"id"`
				MaxMembers    int     `json:"max_members"`
				PlaybookID    string  `json:"playbook_id"`
				PromptVersion int     `json:"prompt_version"`
				StartedAt     *string `json:"started_at"`
				Status        string  `json:"status"`
				Succeeded     int     `json:"succeeded"`
			} `json:"runs"`
		}
		if err := json.Unmarshal(fxBody(t, fx), &page); err != nil {
			t.Fatal(err)
		}
		for _, r := range page.Runs {
			all = append(all, runJSON{r.Attempted, r.Succeeded, r.Failed, r.PromptVersion, r.MaxMembers,
				r.AudienceID, r.ID, r.PlaybookID, r.Status, r.CreatedAt, r.Error, r.StartedAt, r.FinishedAt})
		}
	}
	for _, r := range all {
		ws := "ws-alpha"
		if r.PlaybookID == "pb-gov-b" {
			ws = "ws-beta"
		}
		e.exec(t, `INSERT INTO playbook_runs (id, workspace_id, playbook_id, audience_id, status, prompt_version, prompt_snapshot, max_members,
			attempted, succeeded, failed, steps_snapshot, error, started_at, finished_at, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,'p',$7,$8,$9,$10,'[]',$11,$12::timestamp,$13::timestamp,$14::timestamp)`,
			r.ID, ws, r.PlaybookID, r.AudienceID, r.Status, r.PromptVersion, r.MaxMembers, r.Attempted, r.Succeeded, r.Failed,
			r.Error, r.StartedAt, r.FinishedAt, r.CreatedAt)
		if r.Status == "pending" || r.Status == "running" {
			e.exec(t, `INSERT INTO jobs (type, payload, status, priority, created_at, retry_count, max_retries, next_run_at, fire_key, workspace_id, executor)
				VALUES ('research_playbook_run', $1::json, 'pending', 1, now(), 0, 3, now(), $2, $3, 'python')`,
				fmt.Sprintf(`{"workspace_id":"%s","run_id":"%s"}`, ws, r.ID), "playbook:"+r.ID, ws)
		}
	}
	for _, fx := range []string{"pb_results_r1_default", "pb_results_r2_with_failed", "pb_results_beta_run"} {
		var page struct {
			Results []struct {
				Attempts  int             `json:"attempts"`
				CreatedAt string          `json:"created_at"`
				Error     *string         `json:"error"`
				ID        int             `json:"id"`
				LeadID    int             `json:"lead_id"`
				Metadata  json.RawMessage `json:"metadata"`
				RunID     string          `json:"run_id"`
				Status    string          `json:"status"`
				UpdatedAt string          `json:"updated_at"`
				Value     string          `json:"value"`
			} `json:"results"`
		}
		if err := json.Unmarshal(fxBody(t, fx), &page); err != nil {
			t.Fatal(err)
		}
		for _, r := range page.Results {
			ws := "ws-alpha"
			if strings.HasPrefix(r.RunID, "run-gov-b") {
				ws = "ws-beta"
			}
			e.exec(t, `INSERT INTO playbook_results (id, workspace_id, run_id, lead_id, status, value, result_metadata, error, attempts, created_at, updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7::json,$8,$9,$10::timestamp,$11::timestamp)`,
				r.ID, ws, r.RunID, r.LeadID, r.Status, r.Value, string(r.Metadata), r.Error, r.Attempts, r.CreatedAt, r.UpdatedAt)
		}
	}
	// the explicit ids above must not collide with what the engine inserts later
	e.exec(t, `SELECT setval(pg_get_serial_sequence('playbook_results','id'), GREATEST(1000, (SELECT last_value FROM playbook_results_id_seq), (SELECT coalesce(max(id), 0) FROM playbook_results)))`)
}

type runCase struct{ name, who, ws string }

// runCases is the recorded order of the run route cases: the reads that predate the two starts, the starts, the
// reads that see the started runs, then the retries and the cancels (each builds on the previous state).
var runCases = []runCase{
	{"pb_run_start_bob_403", "bob", "ws-alpha"},
	{"pb_run_start_missing_audience_422", "alice", "ws-alpha"},
	{"pb_run_start_max_members_0_422", "alice", "ws-alpha"},
	{"pb_run_start_max_members_1001_422", "alice", "ws-alpha"},
	{"pb_run_start_unknown_playbook_404", "alice", "ws-alpha"},
	{"pb_run_start_foreign_playbook_404", "alice", "ws-alpha"},
	{"pb_run_start_disabled_409", "alice", "ws-alpha"},
	{"pb_run_start_unknown_audience_404", "alice", "ws-alpha"},
	{"pb_run_start_foreign_audience_404", "alice", "ws-alpha"},
	{"pb_runs_multi_default", "alice", "ws-alpha"},
	{"pb_runs_sched_default", "alice", "ws-alpha"},
	{"pb_runs_beta_playbook", "bob", "ws-beta"},
	{"pb_runs_foreign_playbook_404", "alice", "ws-alpha"},
	{"pb_runs_unknown_playbook_404", "alice", "ws-alpha"},
	{"pb_runs_limit_0_422", "alice", "ws-alpha"},
	{"pb_runs_limit_201_422", "alice", "ws-alpha"},
	{"pb_runs_offset_100001_422", "alice", "ws-alpha"},
	{"pb_runs_cursor_forged_422", "alice", "ws-alpha"},
	{"pb_runs_cursor_too_long_422", "alice", "ws-alpha"},
	{"pb_runs_limit3", "alice", "ws-alpha"},
	{"pb_runs_limit3_cursor", "alice", "ws-alpha"},
	{"pb_runs_offset2", "alice", "ws-alpha"},
	{"pb_runs_offset_ignored_with_cursor", "alice", "ws-alpha"},
	{"pb_results_r1_default", "alice", "ws-alpha"},
	{"pb_results_r1_limit2", "alice", "ws-alpha"},
	{"pb_results_r1_offset3", "alice", "ws-alpha"},
	{"pb_results_r1_cursor", "alice", "ws-alpha"},
	{"pb_results_r1_cursor_last", "alice", "ws-alpha"},
	{"pb_results_cursor_zero", "alice", "ws-alpha"},
	{"pb_results_cascade_after_delete", "alice", "ws-alpha"},
	{"pb_results_r2_with_failed", "alice", "ws-alpha"},
	{"pb_results_r4_empty", "alice", "ws-alpha"},
	{"pb_results_beta_run", "bob", "ws-beta"},
	{"pb_results_foreign_run_404", "alice", "ws-alpha"},
	{"pb_results_unknown_run_404", "alice", "ws-alpha"},
	{"pb_results_limit_0_422", "alice", "ws-alpha"},
	{"pb_results_limit_501_422", "alice", "ws-alpha"},
	{"pb_results_cursor_abc_422", "alice", "ws-alpha"},
	{"pb_results_cursor_negative_422", "alice", "ws-alpha"},
	{"pb_results_cursor_too_long_422", "alice", "ws-alpha"},
	{"pb_runs_bob_viewer_alpha", "bob", "ws-alpha"},
	{"pb_run_start_202", "alice", "ws-alpha"},
	{"pb_run_start_default_members_202", "alice", "ws-alpha"},
	{"pb_runs_after_start", "alice", "ws-alpha"},
	{"pb_retry_bob_403", "bob", "ws-alpha"},
	{"pb_retry_unknown_404", "alice", "ws-alpha"},
	{"pb_retry_foreign_404", "alice", "ws-alpha"},
	{"pb_retry_completed_409", "alice", "ws-alpha"},
	{"pb_retry_pending_409", "alice", "ws-alpha"},
	{"pb_retry_running_409", "alice", "ws-alpha"},
	{"pb_retry_cancelled_202", "alice", "ws-alpha"},
	{"pb_retry_failed_202", "alice", "ws-alpha"},
	{"pb_retry_with_errors_202", "alice", "ws-alpha"},
	{"pb_cancel_bob_403", "bob", "ws-alpha"},
	{"pb_cancel_unknown_404", "alice", "ws-alpha"},
	{"pb_cancel_foreign_404", "alice", "ws-alpha"},
	{"pb_cancel_pending_200", "alice", "ws-alpha"},
	{"pb_cancel_cancelled_409", "alice", "ws-alpha"},
	{"pb_cancel_completed_409", "alice", "ws-alpha"},
	{"pb_cancel_running_200", "alice", "ws-alpha"},
	{"pb_cancel_retried_200", "alice", "ws-alpha"},
}

func runVolatile(name string) []string {
	switch {
	case strings.HasPrefix(name, "pb_run_start_") && strings.HasSuffix(name, "_202"):
		return []string{"$.id", "$.created_at"}
	case name == "pb_runs_after_start":
		return []string{"$.runs[0].id", "$.runs[0].created_at", "$.runs[1].id", "$.runs[1].created_at"}
	case strings.HasPrefix(name, "pb_cancel_") && strings.HasSuffix(name, "_200"):
		return []string{"$.finished_at"}
	}
	return nil
}

func TestRunRoutesMatchFixtures(t *testing.T) {
	e := newPBRunEnv(t, nil)
	for _, c := range runCases {
		fx := contract.Load(t, "playbooks/"+c.name)
		body := ""
		if len(fx.Request.Body) > 0 && string(fx.Request.Body) != "null" {
			body = string(fx.Request.Body)
		}
		_, got, _ := e.req(c.who, c.ws, fx.Request.Method, fx.Request.Path, body)
		t.Run(c.name, func(t *testing.T) {
			contract.AssertEqual(t, got, fx.Response, contract.IgnoreShapeOf(runVolatile(c.name)...))
		})
	}
}

func (e pbEnv) runJobs(t *testing.T, runID string) int {
	return e.scalar(t, `SELECT count(*) FROM jobs WHERE type='research_playbook_run' AND fire_key=$1`, "playbook:"+runID)
}

func TestStartRunDisabledAndForeignAudience(t *testing.T) {
	e := newPBRunEnv(t, nil)
	runs := e.scalar(t, `SELECT count(*) FROM playbook_runs`)
	jobs := e.scalar(t, `SELECT count(*) FROM jobs WHERE type='research_playbook_run'`)
	for _, c := range []struct {
		path, body, want string
		code             int
	}{
		{"pb-gov-off", `{"audience_id":"aud-gov-1"}`, `{"detail":"Playbook is disabled"}`, 409},
		{"pb-gov-multi", `{"audience_id":"aud-gov-b"}`, `{"detail":"Audience not found"}`, 404},
		{"pb-gov-multi", `{"audience_id":"nope"}`, `{"detail":"Audience not found"}`, 404},
		{"pb-gov-b", `{"audience_id":"aud-gov-1"}`, `{"detail":"Playbook not found"}`, 404},
		// the playbook is checked before the audience, the disabled check before the audience lookup
		{"pb-gov-off", `{"audience_id":"nope"}`, `{"detail":"Playbook is disabled"}`, 409},
	} {
		code, _, raw := e.req("alice", "ws-alpha", "POST", pbBase+"/"+c.path+"/runs", c.body)
		if code != c.code || !jsonEqual(raw, c.want) {
			t.Errorf("%s %s: %d %s", c.path, c.body, code, raw)
		}
	}
	if n := e.scalar(t, `SELECT count(*) FROM playbook_runs`); n != runs {
		t.Errorf("rejected starts wrote runs: %d -> %d", runs, n)
	}
	if n := e.scalar(t, `SELECT count(*) FROM jobs WHERE type='research_playbook_run'`); n != jobs {
		t.Errorf("rejected starts enqueued jobs: %d -> %d", jobs, n)
	}
}

func TestStartRunMaxMembersBounds422(t *testing.T) {
	e := newPBRunEnv(t, nil)
	for _, c := range []struct {
		body string
		code int
	}{
		{`{"audience_id":"aud-gov-1","max_members":1}`, 202},
		{`{"audience_id":"aud-gov-1","max_members":1000}`, 202},
		{`{"audience_id":"aud-gov-1","max_members":0}`, 422},
		{`{"audience_id":"aud-gov-1","max_members":1001}`, 422},
		{`{"audience_id":"aud-gov-1","max_members":-5}`, 422},
		{`{"audience_id":"aud-gov-1","max_members":"abc"}`, 422},
		{`{"audience_id":"aud-gov-1","max_members":1.5}`, 422},
		{`{"audience_id":"aud-gov-1","max_members":null}`, 422},
		{`{"audience_id":7}`, 422},
		{`{"audience_id":null}`, 422},
		{``, 422},
		{`[]`, 422},
		{`{"audience_id":"aud-gov-1"`, 422},
	} {
		code, _, raw := e.req("alice", "ws-alpha", "POST", pbBase+"/pb-gov-multi/runs", c.body)
		if code != c.code {
			t.Errorf("%q: %d %s", c.body, code, raw)
		}
	}
	// 422 wins over the 404 of an unknown playbook (FastAPI validates the body first)
	if code, _, raw := e.req("alice", "ws-alpha", "POST", pbBase+"/nope/runs", `{"audience_id":"aud-gov-1","max_members":0}`); code != 422 {
		t.Errorf("unknown playbook with bad body: %d %s", code, raw)
	}
	if n := e.scalar(t, `SELECT count(*) FROM playbook_runs WHERE max_members IN (1,1000)`); n != 2 {
		t.Errorf("runs at the bounds: %d, want 2", n)
	}
	if n := e.scalar(t, `SELECT count(*) FROM playbook_runs WHERE workspace_id='ws-alpha' AND requested_by IS NOT NULL AND requested_by <> ''`); n != 2 {
		t.Errorf("requested_by not recorded on the started runs: %d", n)
	}
}

func TestStartRunCreatesRunAndJobAtomically(t *testing.T) {
	e := newPBRunEnv(t, nil)
	code, resp, raw := e.req("alice", "ws-alpha", "POST", pbBase+"/pb-gov-multi/runs", `{"audience_id":"aud-gov-1","max_members":7}`)
	if code != 202 {
		t.Fatalf("%d %s", code, raw)
	}
	var run struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(resp.Body, &run)
	if n := e.runJobs(t, run.ID); n != 1 {
		t.Fatalf("jobs for the run: %d", n)
	}
	if got := e.text(t, `SELECT status||'|'||executor||'|'||priority||'|'||max_retries||'|'||workspace_id FROM jobs WHERE fire_key=$1`, "playbook:"+run.ID); got != "pending|go|1|3|ws-alpha" {
		t.Errorf("job: %s", got)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(e.text(t, `SELECT payload::text FROM jobs WHERE fire_key=$1`, "playbook:"+run.ID)), &payload); err != nil ||
		payload["workspace_id"] != "ws-alpha" || payload["run_id"] != run.ID || len(payload) != 2 {
		t.Errorf("payload: %v %v", payload, err)
	}
	if got := e.text(t, `SELECT prompt_version||'|'||prompt_snapshot||'|'||max_members||'|'||status FROM playbook_runs WHERE id=$1`, run.ID); got != "6|Research {company} and summarise.|7|pending" {
		t.Errorf("run snapshot: %s", got)
	}
	if got := e.text(t, `SELECT steps_snapshot::text FROM playbook_runs WHERE id=$1`, run.ID); !strings.Contains(got, `"key":"fit"`) {
		t.Errorf("steps snapshot: %s", got)
	}

	// The cohort picks the executor (GO_EXECUTOR_ENABLED, GO_EXECUTOR_PLAYBOOKS, GO_EXECUTOR_WORKSPACES).
	goEnv := e.withRunRoutes(func(k string) string {
		return map[string]string{"GO_EXECUTOR_ENABLED": "true", "GO_EXECUTOR_WORKSPACES": "ws-alpha"}[k]
	})
	_, resp, raw = goEnv.req("alice", "ws-alpha", "POST", pbBase+"/pb-gov-multi/runs", `{"audience_id":"aud-gov-1"}`)
	_ = json.Unmarshal(resp.Body, &run)
	if got := goEnv.text(t, `SELECT executor FROM jobs WHERE fire_key=$1`, "playbook:"+run.ID); got != "go" {
		t.Errorf("cohort executor: %q (%s)", got, raw)
	}

	// Atomic: when the job insert fails the run row is not left behind (and the answer is a 500, no detail leaked).
	e.exec(t, `CREATE OR REPLACE FUNCTION t17_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 't17 boom secret'; END $$`)
	e.exec(t, `CREATE TRIGGER t17_fail BEFORE INSERT ON jobs FOR EACH ROW WHEN (NEW.type = 'research_playbook_run') EXECUTE FUNCTION t17_fail()`)
	t.Cleanup(func() {
		_, _ = e.owner.Exec(context.Background(), `DROP TRIGGER IF EXISTS t17_fail ON jobs`)
	})
	before := e.scalar(t, `SELECT count(*) FROM playbook_runs`)
	code, _, raw = e.req("alice", "ws-alpha", "POST", pbBase+"/pb-gov-multi/runs", `{"audience_id":"aud-gov-1"}`)
	if code != 500 || strings.Contains(raw, "boom") {
		t.Errorf("failing enqueue: %d %s", code, raw)
	}
	if n := e.scalar(t, `SELECT count(*) FROM playbook_runs`); n != before {
		t.Errorf("a failed enqueue left a run behind: %d -> %d", before, n)
	}
	// retry is atomic too: the run stays failed
	code, _, _ = e.req("alice", "ws-alpha", "POST", pbBase+"/runs/run-gov-r3/retry", "")
	if code != 500 {
		t.Errorf("failing retry enqueue: %d", code)
	}
	if got := e.text(t, `SELECT status FROM playbook_runs WHERE id='run-gov-r3'`); got != "failed" {
		t.Errorf("a failed enqueue left the run %q", got)
	}
}

func TestResultsCursorMalformed422(t *testing.T) {
	e := newPBRunEnv(t, nil)
	for _, c := range []struct {
		cursor string
		code   int
	}{
		{"abc", 422}, {"-1", 422}, {"1.5", 422}, {"0x10", 422}, {"1e3", 422}, {"%20", 422}, {"--1", 422}, {"1__0", 422},
		{"0", 200}, {"+2", 200}, {"1_0", 200}, {"%202%20", 200}, {"99999999999999999999", 200},
		{strings.Repeat("1", 32), 200}, {strings.Repeat("1", 33), 422},
	} {
		code, _, raw := e.req("alice", "ws-alpha", "GET", pbBase+"/runs/run-gov-r1/results?cursor="+c.cursor, "")
		if code != c.code {
			t.Errorf("cursor %q: %d %s", c.cursor, code, raw)
		}
		if c.code == 422 && c.cursor != strings.Repeat("1", 33) && !jsonEqual(raw, `{"detail":"invalid playbook result cursor"}`) {
			t.Errorf("cursor %q body: %s", c.cursor, raw)
		}
	}
	// an empty cursor means no cursor
	if code, resp, _ := e.req("alice", "ws-alpha", "GET", pbBase+"/runs/run-gov-r1/results?cursor=&limit=1", ""); code != 200 || !strings.Contains(string(resp.Body), `"offset":0`) {
		t.Errorf("empty cursor: %d %s", code, resp.Body)
	}
	// the cursor check comes after the 404 for an unknown run
	if code, _, _ := e.req("alice", "ws-alpha", "GET", pbBase+"/runs/nope/results?cursor=abc", ""); code != 404 {
		t.Errorf("unknown run with bad cursor: %d", code)
	}
}

func (e pbEnv) runState(t *testing.T, id string) string {
	return e.text(t, `SELECT status||'|'||coalesce(error,'')||'|'||(finished_at IS NOT NULL)::text||'|'||attempted||'|'||failed FROM playbook_runs WHERE id=$1`, id)
}

func TestRetryOnlyTerminalStatesResetsRunAndEnqueues(t *testing.T) {
	e := newPBRunEnv(t, nil)
	for _, c := range []struct {
		run, state string
		code       int
	}{
		{"run-gov-r1", "completed", 409},
		{"run-gov-r5", "pending", 409},
		{"run-gov-r6", "running", 409},
	} {
		before := e.runJobs(t, c.run)
		code, _, raw := e.req("alice", "ws-alpha", "POST", pbBase+"/runs/"+c.run+"/retry", "")
		if code != c.code || !jsonEqual(raw, `{"detail":"Only failed, completed-with-errors, or cancelled runs can be retried"}`) {
			t.Errorf("%s (%s): %d %s", c.run, c.state, code, raw)
		}
		if n := e.runJobs(t, c.run); n != before {
			t.Errorf("%s: a refused retry enqueued a job", c.run)
		}
	}
	for _, run := range []string{"run-gov-r2", "run-gov-r3", "run-gov-r4"} {
		before := e.runJobs(t, run)
		code, resp, raw := e.req("alice", "ws-alpha", "POST", pbBase+"/runs/"+run+"/retry", "")
		if code != 202 {
			t.Fatalf("%s: %d %s", run, code, raw)
		}
		var r struct {
			Status     string  `json:"status"`
			Error      *string `json:"error"`
			FinishedAt *string `json:"finished_at"`
		}
		_ = json.Unmarshal(resp.Body, &r)
		if r.Status != "pending" || r.Error != nil || r.FinishedAt != nil {
			t.Errorf("%s: %s", run, resp.Body)
		}
		if n := e.runJobs(t, run); n != before+1 {
			t.Errorf("%s: jobs %d -> %d, want one more", run, before, n)
		}
		if got := e.text(t, `SELECT status||'|'||executor||'|'||payload::text FROM jobs WHERE fire_key=$1 ORDER BY id DESC LIMIT 1`, "playbook:"+run); !strings.HasPrefix(got, "pending|go|") || !strings.Contains(got, run) {
			t.Errorf("%s: job %s", run, got)
		}
	}
	// counts are kept (the engine resumes from the ledger), and a second retry is refused: the run is pending again
	if got := e.runState(t, "run-gov-r3"); got != "pending||false|2|2" {
		t.Errorf("r3 after retry: %s", got)
	}
	if code, _, _ := e.req("alice", "ws-alpha", "POST", pbBase+"/runs/run-gov-r3/retry", ""); code != 409 {
		t.Errorf("double retry: %d", code)
	}
}

// resCalls records the leads a fake researcher is asked about.
type resCalls struct {
	mu    sync.Mutex
	leads []int
	fn    func(n int)
}

func (r *resCalls) Research(ctx context.Context, ws string, req playbooks.ResearchRequest) (playbooks.ResearchResult, error) {
	r.mu.Lock()
	r.leads = append(r.leads, int(asFloat(req.Context["lead"])))
	n := len(r.leads)
	r.mu.Unlock()
	if r.fn != nil {
		r.fn(n)
	}
	return playbooks.ResearchResult{Success: true, Value: "fresh"}, nil
}

func asFloat(v any) float64 {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case float64:
		return x
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return -1
}

func (e pbEnv) members(t *testing.T, run string, leads ...int) {
	for _, l := range leads {
		e.exec(t, `INSERT INTO audience_members (workspace_id, audience_id, lead_id, snapshot) VALUES ('ws-alpha','aud-gov-1',$1,$2::json)`,
			l, fmt.Sprintf(`{"lead": %d, "name": "L%d"}`, l, l))
	}
}

func TestRetryDoesNotRerunSuccessResults(t *testing.T) {
	e := newPBRunEnv(t, nil)
	// run-gov-r2 holds recorded results: leads 4 and 5 succeeded, lead 6 failed.
	e.exec(t, `DELETE FROM audience_members WHERE workspace_id='ws-alpha'`)
	e.members(t, "run-gov-r2", 4, 5, 6)
	if code, _, raw := e.req("alice", "ws-alpha", "POST", pbBase+"/runs/run-gov-r2/retry", ""); code != 202 {
		t.Fatalf("%d %s", code, raw)
	}
	fake := &resCalls{}
	if err := playbooks.HandleRun(context.Background(), playbooks.Deps{Pool: e.pool, Researcher: fake}, "ws-alpha", "run-gov-r2"); err != nil {
		t.Fatal(err)
	}
	if len(fake.leads) == 0 {
		t.Fatal("the failed lead was not retried")
	}
	for _, l := range fake.leads {
		if l == 4 || l == 5 {
			t.Errorf("a lead with a success result was researched again: %v", fake.leads)
		}
	}
	if got := e.text(t, `SELECT value FROM playbook_results WHERE run_id='run-gov-r2' AND lead_id=4`); got != "ok" {
		t.Errorf("success result rewritten: %q", got)
	}
	if got := e.text(t, `SELECT status||'|'||succeeded||'|'||failed FROM playbook_runs WHERE id='run-gov-r2'`); got != "completed|3|0" {
		t.Errorf("run after the engine: %s", got)
	}
}

func TestCancelMarksRunAndJobCancelled(t *testing.T) {
	e := newPBRunEnv(t, nil)
	// run-gov-r6 is mid-flight: its job is being processed; an older job of the same run is already done.
	e.exec(t, `UPDATE jobs SET status='processing' WHERE fire_key='playbook:run-gov-r6'`)
	e.exec(t, `INSERT INTO jobs (type, payload, status, priority, created_at, retry_count, max_retries, next_run_at, fire_key, workspace_id, executor, error)
		VALUES ('research_playbook_run', '{}', 'completed', 1, now(), 0, 3, now(), 'playbook:run-gov-r6', 'ws-alpha', 'python', 'old')`)
	code, _, raw := e.req("alice", "ws-alpha", "POST", pbBase+"/runs/run-gov-r6/cancel", "")
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	if got := e.runState(t, "run-gov-r6"); got != "cancelled|Cancelled by user|true|2|0" {
		t.Errorf("run: %s", got)
	}
	if got := e.text(t, `SELECT status||'|'||coalesce(error,'') FROM jobs WHERE fire_key='playbook:run-gov-r6' AND status<>'completed'`); got != "cancelled|Cancelled by user" {
		t.Errorf("processing job: %s", got)
	}
	if got := e.text(t, `SELECT status||'|'||error FROM jobs WHERE fire_key='playbook:run-gov-r6' AND error='old'`); got != "completed|old" {
		t.Errorf("finished job touched: %s", got)
	}
	// siblings: another run's job and the playbook's schedule job are untouched
	if got := e.text(t, `SELECT status FROM jobs WHERE fire_key='playbook:run-gov-r7'`); got != "pending" {
		t.Errorf("sibling job %s", got)
	}
	if n := pendingSchedule(e, t, "pb-gov-sched"); n != 1 {
		t.Errorf("schedule job %d pending", n)
	}
	// a pending run with its pending job
	if code, _, raw := e.req("alice", "ws-alpha", "POST", pbBase+"/runs/run-gov-r5/cancel", ""); code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	if got := e.text(t, `SELECT status||'|'||error FROM jobs WHERE fire_key='playbook:run-gov-r5'`); got != "cancelled|Cancelled by user" {
		t.Errorf("pending job: %s", got)
	}
}

func TestCancelAfterCompleteIs409(t *testing.T) {
	e := newPBRunEnv(t, nil)
	for _, run := range []string{"run-gov-r1", "run-gov-r2", "run-gov-r3", "run-gov-r4"} {
		before := e.runState(t, run)
		code, _, raw := e.req("alice", "ws-alpha", "POST", pbBase+"/runs/"+run+"/cancel", "")
		if code != 409 || !jsonEqual(raw, `{"detail":"Only pending or running playbook runs can be cancelled"}`) {
			t.Errorf("%s: %d %s", run, code, raw)
		}
		if got := e.runState(t, run); got != before {
			t.Errorf("%s changed: %s -> %s", run, before, got)
		}
	}
	// cancelling twice: the second is a 409 and leaves the first finished_at alone
	if code, _, _ := e.req("alice", "ws-alpha", "POST", pbBase+"/runs/run-gov-r5/cancel", ""); code != 200 {
		t.Fatal(code)
	}
	at := e.text(t, `SELECT finished_at::text FROM playbook_runs WHERE id='run-gov-r5'`)
	if code, _, _ := e.req("alice", "ws-alpha", "POST", pbBase+"/runs/run-gov-r5/cancel", ""); code != 409 {
		t.Errorf("second cancel: %d", code)
	}
	if got := e.text(t, `SELECT finished_at::text FROM playbook_runs WHERE id='run-gov-r5'`); got != at {
		t.Errorf("finished_at moved: %s -> %s", at, got)
	}
}

// A cancel that lands while the engine is mid-run is never overwritten by the engine's final update.
func TestCancelRacingTheEngineSticks(t *testing.T) {
	e := newPBRunEnv(t, nil)
	e.exec(t, `DELETE FROM audience_members WHERE workspace_id='ws-alpha'`)
	e.members(t, "run-gov-r5", 21, 22, 23)
	fake := &resCalls{fn: func(n int) {
		if n == 1 {
			if code, _, raw := e.req("alice", "ws-alpha", "POST", pbBase+"/runs/run-gov-r5/cancel", ""); code != 200 {
				t.Errorf("cancel during the run: %d %s", code, raw)
			}
		}
	}}
	if err := playbooks.HandleRun(context.Background(), playbooks.Deps{Pool: e.pool, Researcher: fake}, "ws-alpha", "run-gov-r5"); err != nil {
		t.Fatal(err)
	}
	if got := e.text(t, `SELECT status||'|'||error FROM playbook_runs WHERE id='run-gov-r5'`); got != "cancelled|Cancelled by user" {
		t.Errorf("engine overwrote the cancel: %s", got)
	}
	if len(fake.leads) != 1 {
		t.Errorf("engine kept researching after the cancel: %v", fake.leads)
	}
}

func (e pbEnv) lastRunAudit(t *testing.T, path string) (status int, meta map[string]any) {
	t.Helper()
	var raw string
	if err := e.owner.QueryRow(context.Background(), `SELECT response_status, metadata_json::text FROM governance_audit_events
		WHERE workspace_id='ws-alpha' AND resource_path=$1 ORDER BY created_at DESC, id DESC LIMIT 1`, path).Scan(&status, &raw); err != nil {
		t.Fatalf("no audit event for %s: %v", path, err)
	}
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatal(err)
	}
	return
}

func TestRetryCancelAuditMetadataKeys(t *testing.T) {
	e := newPBRunEnv(t, nil)
	e.req("alice", "ws-alpha", "POST", pbBase+"/runs/run-gov-r2/retry", "")
	st, meta := e.lastRunAudit(t, pbBase+"/runs/run-gov-r2/retry")
	want := map[string]any{"action": "research_playbook.run.retry", "playbook_id": "pb-gov-multi", "run_id": "run-gov-r2", "previous_failed": float64(1)}
	if st != 202 || fmt.Sprint(meta) != fmt.Sprint(want) {
		t.Errorf("retry audit: %d %v", st, meta)
	}
	e.req("alice", "ws-alpha", "POST", pbBase+"/runs/run-gov-r6/cancel", "")
	st, meta = e.lastRunAudit(t, pbBase+"/runs/run-gov-r6/cancel")
	want = map[string]any{"action": "research_playbook.run.cancel", "playbook_id": "pb-gov-sched", "run_id": "run-gov-r6"}
	if st != 200 || fmt.Sprint(meta) != fmt.Sprint(want) {
		t.Errorf("cancel audit: %d %v", st, meta)
	}
	// refused calls carry no metadata (Python sets it only on success)
	e.req("alice", "ws-alpha", "POST", pbBase+"/runs/run-gov-r1/retry", "")
	if st, meta := e.lastRunAudit(t, pbBase+"/runs/run-gov-r1/retry"); st != 409 || len(meta) != 0 {
		t.Errorf("refused retry audit: %d %v", st, meta)
	}
	e.req("alice", "ws-alpha", "POST", pbBase+"/runs/run-gov-r1/cancel", "")
	if st, meta := e.lastRunAudit(t, pbBase+"/runs/run-gov-r1/cancel"); st != 409 || len(meta) != 0 {
		t.Errorf("refused cancel audit: %d %v", st, meta)
	}
}

func TestRoutePrecedenceRunsVsPlaybookId(t *testing.T) {
	e := newPBRunEnv(t, nil)
	// A playbook whose id is "runs" and a run whose id is "runs" are both reachable, and the static
	// /runs/... paths still answer for the real runs.
	e.exec(t, `INSERT INTO research_playbooks (id, workspace_id, name, description, prompt_template, steps, output_format, max_steps, cell_budget_usd, version, enabled, created_at, updated_at)
		VALUES ('runs','ws-alpha','Runs named','', 'Describe {company} please.', '[]', 'text', 1, 0.1, 1, true, now(), now())`)
	e.exec(t, `INSERT INTO playbook_runs (id, workspace_id, playbook_id, audience_id, status, prompt_version, prompt_snapshot, max_members, attempted, succeeded, failed, steps_snapshot)
		VALUES ('runs','ws-alpha','runs','aud-gov-1','completed',1,'p',100,0,0,0,'[]')`)
	if code, resp, raw := e.req("alice", "ws-alpha", "GET", pbBase+"/runs/runs", ""); code != 200 || !strings.Contains(string(resp.Body), `"id":"runs"`) {
		t.Errorf("GET /runs/runs (playbook id runs): %d %s", code, raw)
	}
	if code, resp, raw := e.req("alice", "ws-alpha", "GET", pbBase+"/runs/runs/results", ""); code != 200 || !strings.Contains(string(resp.Body), `"results":[]`) {
		t.Errorf("GET /runs/runs/results: %d %s", code, raw)
	}
	if code, _, raw := e.req("alice", "ws-alpha", "GET", pbBase+"/runs/run-gov-r1/results?limit=1", ""); code != 200 {
		t.Errorf("results of a real run: %d %s", code, raw)
	}
	if code, _, raw := e.req("alice", "ws-alpha", "POST", pbBase+"/runs/runs", `{"audience_id":"aud-gov-1"}`); code != 202 {
		t.Errorf("POST /runs/runs (start on playbook runs): %d %s", code, raw)
	}
	// the CRUD routes stay reachable beside them, and unrelated verbs are not routed
	if code, _, _ := e.req("alice", "ws-alpha", "PATCH", pbBase+"/runs", `{"description":"x"}`); code != 200 {
		t.Errorf("PATCH /{id}: %d", code)
	}
	if code, _, _ := e.req("alice", "ws-alpha", "DELETE", pbBase+"/runs/run-gov-r1/results", ""); code != http.StatusMethodNotAllowed && code != 404 {
		t.Errorf("DELETE results: %d", code)
	}
}

func TestRunRoutesTenantIsolation404(t *testing.T) {
	e := newPBRunEnv(t, nil)
	snapshot := func() string {
		return e.text(t, `SELECT string_agg(id||status||coalesce(error,''), ',' ORDER BY id) FROM playbook_runs WHERE workspace_id='ws-alpha'`) +
			fmt.Sprint(e.scalar(t, `SELECT count(*) FROM jobs WHERE workspace_id='ws-alpha' AND type='research_playbook_run' AND status='pending'`))
	}
	before := snapshot()
	for _, c := range []struct{ method, path, body, detail string }{
		{"GET", "/runs/run-gov-r1/results", "", "Playbook run not found"},
		{"POST", "/runs/run-gov-r3/retry", "", "Playbook run not found"},
		{"POST", "/runs/run-gov-r5/cancel", "", "Playbook run not found"},
		{"GET", "/pb-gov-multi/runs", "", "Playbook not found"},
		{"POST", "/pb-gov-multi/runs", `{"audience_id":"aud-gov-b"}`, "Playbook not found"},
		{"POST", "/pb-gov-b/runs", `{"audience_id":"aud-gov-1"}`, "Audience not found"},
	} {
		code, _, raw := e.req("bob", "ws-beta", c.method, pbBase+c.path, c.body)
		if code != 404 || !jsonEqual(raw, `{"detail":"`+c.detail+`"}`) {
			t.Errorf("%s %s as bob in ws-beta: %d %s", c.method, c.path, code, raw)
		}
	}
	if after := snapshot(); after != before {
		t.Errorf("a cross-tenant call changed alpha state:\n%s\n%s", before, after)
	}
	// and the other way: alice (not a member of ws-beta) cannot reach beta's run
	if code, _, _ := e.req("alice", "ws-beta", "POST", pbBase+"/runs/run-gov-b1/cancel", ""); code != 403 {
		t.Errorf("alice in ws-beta: %d", code)
	}
}
