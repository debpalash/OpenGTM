//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type pbEnv struct {
	mcpEnv
	owner *pgxpool.Pool
	ids   map[string]int32
}

// newPBEnv seeds the recorded playbook world: alice owner (admin) of ws-alpha, bob a plain member of ws-alpha and
// owner of ws-beta, erin an editor of ws-alpha and no member of ws-beta. ws-alpha holds pb-gov-multi (three steps,
// runs r1-r4 all finished), pb-gov-sched (hourly over aud-gov-1, mirror and pending schedule job, runs r5-r8 active)
// and the disabled pb-gov-off; ws-beta holds pb-gov-b.
func newPBEnv(t *testing.T) pbEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	owner, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID())
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Flags: config.Flags{PGLeadStore: true}}
	registerPlaybooksRoutes(r, d)
	e := pbEnv{mcpEnv: mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}, owner: owner, ids: map[string]int32{}}
	e.purge()
	t.Cleanup(e.purge)
	for _, u := range []struct {
		n     string
		admin bool
	}{{"alice", true}, {"bob", false}, {"erin", false}} {
		role := "user"
		if u.admin {
			role = "admin"
		}
		e.seed(t, u.n, pythonHash, true, role, u.admin)
		e.ids[u.n] = e.mustUserByName(t, u.n).ID
	}
	e.workspace(t, "ws-alpha", e.ids["alice"])
	e.workspace(t, "ws-beta", e.ids["bob"])
	e.member(t, "ws-alpha", e.ids["alice"], "owner")
	e.member(t, "ws-alpha", e.ids["bob"], "member")
	e.member(t, "ws-alpha", e.ids["erin"], "editor")
	e.member(t, "ws-beta", e.ids["bob"], "owner")
	e.seedWorld(t)
	return e
}

func (e pbEnv) purge() {
	ctx := context.Background()
	c, err := e.owner.Acquire(ctx)
	if err != nil {
		return
	}
	defer c.Release()
	_, _ = c.Exec(ctx, `SET session_replication_role = replica`)
	for _, tb := range []string{"playbook_results", "playbook_runs", "playbook_schedules", "research_playbooks", "audiences"} {
		_, _ = c.Exec(ctx, `DELETE FROM `+tb+` WHERE workspace_id IN ('ws-alpha','ws-beta')`)
	}
	_, _ = c.Exec(ctx, `DELETE FROM jobs WHERE workspace_id IN ('ws-alpha','ws-beta') AND type LIKE 'research_playbook%'`)
	_, _ = c.Exec(ctx, `RESET session_replication_role`)
}

func (e pbEnv) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	c, err := e.owner.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	if _, err := c.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	defer c.Exec(ctx, `RESET session_replication_role`)
	if _, err := c.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (e pbEnv) scalar(t *testing.T, sql string, args ...any) (n int) {
	t.Helper()
	if err := e.owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (e pbEnv) text(t *testing.T, sql string, args ...any) (s string) {
	t.Helper()
	if err := e.owner.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return s
}

func (e pbEnv) seedWorld(t *testing.T) {
	t.Helper()
	for _, a := range [][2]string{{"aud-gov-1", "ws-alpha"}, {"aud-gov-b", "ws-beta"}} {
		e.exec(t, `INSERT INTO audiences (id, workspace_id, name, filters) VALUES ($1,$2,$1,'{}')`, a[0], a[1])
	}
	steps := `[{"key":"research","name":"Research","prompt_template":"Research {company} and summarise what they sell.","output_format":"text"},` +
		`{"key":"fit","name":"Fit score","prompt_template":"Given {research}, rate fit for {company} as JSON.","output_format":"json"},` +
		`{"key":"opener","name":"Opener","prompt_template":"Write a one line opener for {company} using {fit}.","output_format":"text"}]`
	ins := `INSERT INTO research_playbooks (id, workspace_id, name, description, prompt_template, steps, output_format, max_steps,
		cell_budget_usd, version, enabled, schedule_audience_id, schedule_interval_minutes, next_run_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6::json,$7,$8,$9,$10,$11,NULLIF($12,''),NULLIF($13,0),NULLIF($14,'')::timestamp,$15::timestamp,$16::timestamp)`
	e.exec(t, ins, "pb-gov-multi", "ws-alpha", "Multi step research", "three chained steps", "Research {company} and summarise.", steps,
		"text", 4, 0.3, 3, true, "", 0, "", "2026-08-20 08:00:00", "2026-08-28 08:00:00")
	e.exec(t, ins, "pb-gov-sched", "ws-alpha", "Scheduled research", "hourly over the gov audience", "Find the latest funding news for {company}.", `[]`,
		"text", 2, 0.1, 1, true, "aud-gov-1", 60, "2030-01-01 00:00:00", "2026-08-21 08:00:00", "2026-08-27 08:00:00")
	e.exec(t, ins, "pb-gov-off", "ws-alpha", "Disabled research", "", "Describe {company} in two sentences.", `[]`,
		"json", 1, 0.05, 2, false, "", 0, "", "2026-08-22 08:00:00", "2026-08-26 08:00:00")
	e.exec(t, ins, "pb-gov-b", "ws-beta", "Beta research", "beta tenant", "Summarise {company} for the beta workspace.", `[]`,
		"text", 4, 0.1, 1, true, "", 0, "", "2026-08-23 08:00:00", "2026-08-23 08:00:00")
	e.exec(t, `INSERT INTO playbook_schedules (playbook_id, workspace_id, enabled, next_run_at) VALUES ('pb-gov-sched','ws-alpha',true,'2030-01-01 00:00:00')`)
	e.exec(t, `INSERT INTO jobs (type, payload, status, priority, created_at, retry_count, max_retries, next_run_at, fire_key, workspace_id, executor)
		VALUES ('research_playbook_schedule', '{}', 'pending', 0, now(), 0, 3, '2030-01-01', 'playbook_schedule:pb-gov-sched:2030-01-01T00:00:00+00:00', 'ws-alpha', 'python')`)
	for _, r := range []struct{ id, pb, status string }{
		{"run-gov-r1", "pb-gov-multi", "completed"}, {"run-gov-r2", "pb-gov-multi", "completed_with_errors"},
		{"run-gov-r3", "pb-gov-multi", "failed"}, {"run-gov-r4", "pb-gov-multi", "cancelled"},
		{"run-gov-r5", "pb-gov-sched", "pending"}, {"run-gov-r6", "pb-gov-sched", "running"},
		{"run-gov-r7", "pb-gov-sched", "pending"}, {"run-gov-r8", "pb-gov-sched", "running"},
	} {
		e.exec(t, `INSERT INTO playbook_runs (id, workspace_id, playbook_id, audience_id, status, prompt_version, prompt_snapshot, max_members, attempted, succeeded, failed, steps_snapshot)
			VALUES ($1,'ws-alpha',$2,'aud-gov-1',$3,1,'p',100,0,0,0,'[]')`, r.id, r.pb, r.status)
	}
}

func (e pbEnv) req(who, ws, method, path, body string) (int, contract.Resp, string) {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
		hdr["X-Workspace-Id"] = ws
	}
	ctype := ""
	if body != "" {
		ctype = "application/json"
	}
	rec, resp := e.do(method, path, ctype, body, hdr)
	return rec.Code, resp, rec.Body.String()
}

const pbBase = "/api/research-playbooks"

type pbCase struct {
	name, who, ws string
	// before runs against the world just before the request.
	before func(t *testing.T, e pbEnv)
}

// pbCases is the recorded order of the CRUD cases (the state they build on is part of the contract). The recorded
// Python 500 for `name: null` is deliberately not here: Go answers 422 (D3), see TestPatchNullName422NotPanic.
var pbCases = []pbCase{
	{"pb_list_no_auth_401", "", "", nil},
	{"pb_list_nonmember_403", "erin", "ws-beta", nil},
	{"pb_list_alpha", "alice", "ws-alpha", nil},
	{"pb_list_beta", "bob", "ws-beta", nil},
	{"pb_list_erin", "erin", "ws-alpha", nil},
	{"pb_list_bob_viewer_alpha", "bob", "ws-alpha", nil},
	{"pb_create_no_auth_401", "", "", nil},
	{"pb_create_bob_403", "bob", "ws-alpha", nil},
	{"pb_create_missing_name_422", "alice", "ws-alpha", nil},
	{"pb_create_prompt_short_422", "alice", "ws-alpha", nil},
	{"pb_create_max_steps_0_422", "alice", "ws-alpha", nil},
	{"pb_create_max_steps_9_422", "alice", "ws-alpha", nil},
	{"pb_create_budget_zero_422", "alice", "ws-alpha", nil},
	{"pb_create_bad_step_key_422", "alice", "ws-alpha", nil},
	{"pb_create_dup_keys_422", "alice", "ws-alpha", nil},
	{"pb_create_step_format_xml_422", "alice", "ws-alpha", nil},
	{"pb_create_nine_steps_422", "alice", "ws-alpha", nil},
	{"pb_create_output_format_xml_422", "alice", "ws-alpha", nil},
	{"pb_create_minimal_201", "alice", "ws-alpha", nil},
	{"pb_create_duplicate_409", "alice", "ws-alpha", nil},
	{"pb_create_full_201", "alice", "ws-alpha", nil},
	{"pb_create_same_name_beta_201", "bob", "ws-beta", nil},
	{"pb_list_after_create", "alice", "ws-alpha", nil},
	{"pb_patch_bob_403", "bob", "ws-alpha", nil},
	{"pb_patch_unknown_playbook_404", "alice", "ws-alpha", nil},
	{"pb_patch_foreign_playbook_404", "alice", "ws-alpha", nil},
	{"pb_patch_name_empty_422", "alice", "ws-alpha", nil},
	{"pb_patch_output_format_xml_422", "alice", "ws-alpha", nil},
	{"pb_patch_steps_dup_keys_422", "alice", "ws-alpha", nil},
	{"pb_patch_interval_14_422", "alice", "ws-alpha", nil},
	{"pb_patch_interval_10081_422", "alice", "ws-alpha", nil},
	{"pb_patch_unknown_audience_404", "alice", "ws-alpha", nil},
	{"pb_patch_foreign_audience_404", "alice", "ws-alpha", nil},
	{"pb_patch_name_only", "alice", "ws-alpha", nil},
	{"pb_patch_empty_body", "alice", "ws-alpha", nil},
	{"pb_patch_prompt_version_bump", "alice", "ws-alpha", nil},
	{"pb_patch_budget_version_bump", "alice", "ws-alpha", nil},
	{"pb_patch_steps_version_bump", "alice", "ws-alpha", nil},
	{"pb_patch_description_no_bump", "alice", "ws-alpha", nil},
	{"pb_patch_schedule_interval_30", "alice", "ws-alpha", nil},
	{"pb_patch_interval_max", "alice", "ws-alpha", nil},
	{"pb_patch_interval_min", "alice", "ws-alpha", nil},
	{"pb_patch_schedule_audience_only", "alice", "ws-alpha", nil},
	{"pb_patch_enable_off", "alice", "ws-alpha", nil},
	{"pb_patch_disable_off", "alice", "ws-alpha", nil},
	{"pb_delete_bob_403", "bob", "ws-alpha", nil},
	{"pb_delete_unknown_404", "alice", "ws-alpha", nil},
	{"pb_delete_foreign_404", "alice", "ws-alpha", nil},
	{"pb_delete_active_run_409", "alice", "ws-alpha", nil},
	{"pb_delete_multi_active_409", "alice", "ws-alpha", func(t *testing.T, e pbEnv) {
		e.exec(t, `INSERT INTO playbook_runs (id, workspace_id, playbook_id, audience_id, status, prompt_version, prompt_snapshot, max_members, attempted, succeeded, failed, steps_snapshot)
			VALUES ('run-gov-m1','ws-alpha','pb-gov-multi','aud-gov-1','running',1,'p',100,0,0,0,'[]')`)
	}},
	{"pb_delete_off_204", "alice", "ws-alpha", nil},
	{"pb_delete_off_again_404", "alice", "ws-alpha", nil},
	{"pb_list_final", "alice", "ws-alpha", nil},
}

// pbVolatile lists the JSON paths whose value is clock or uuid dependent in the recorded body.
func pbVolatile(fx contract.Fixture) []string {
	var list []map[string]any
	if json.Unmarshal(fx.Response.Body, &list) == nil {
		var out []string
		for i, row := range list {
			p := fmt.Sprintf("$[%d]", i)
			out = append(out, p+".updated_at", p+".next_run_at")
			if id, _ := row["id"].(string); !strings.HasPrefix(id, "pb-gov-") {
				out = append(out, p+".id", p+".created_at")
			}
		}
		return out
	}
	out := []string{"$.updated_at", "$.next_run_at"}
	if fx.Request.Method == "POST" {
		out = append(out, "$.id", "$.created_at")
	}
	return out
}

func TestPlaybookCrudMatchesFixtures(t *testing.T) {
	e := newPBEnv(t)
	for _, c := range pbCases {
		fx := contract.Load(t, "playbooks/"+c.name)
		body := ""
		if len(fx.Request.Body) > 0 && string(fx.Request.Body) != "null" {
			body = string(fx.Request.Body)
		}
		if c.before != nil {
			c.before(t, e)
		}
		_, got, _ := e.req(c.who, c.ws, fx.Request.Method, fx.Request.Path, body)
		t.Run(c.name, func(t *testing.T) {
			contract.AssertEqual(t, got, fx.Response, contract.IgnoreShapeOf(pbVolatile(fx)...))
		})
	}
}

func TestCreateStepsValidationBodies(t *testing.T) {
	e := newPBEnv(t)
	for _, c := range []struct{ name, body, want string }{
		{"steps not a list", `{"name":"S","prompt_template":"Describe {company} please.","steps":"x"}`,
			`{"detail":[{"type":"list_type","loc":["body","steps"],"msg":"Input should be a valid list","input":"x"}]}`},
		{"step not an object", `{"name":"S","prompt_template":"Describe {company} please.","steps":[1]}`,
			`{"detail":[{"type":"model_attributes_type","loc":["body","steps",0],"msg":"Input should be a valid dictionary or object to extract fields from","input":1}]}`},
		{"step missing fields", `{"name":"S","prompt_template":"Describe {company} please.","steps":[{}]}`,
			`{"detail":[{"type":"missing","loc":["body","steps",0,"key"],"msg":"Field required","input":{}},` +
				`{"type":"missing","loc":["body","steps",0,"name"],"msg":"Field required","input":{}},` +
				`{"type":"missing","loc":["body","steps",0,"prompt_template"],"msg":"Field required","input":{}}]}`},
		{"two top-level errors in field order", `{}`,
			`{"detail":[{"type":"missing","loc":["body","name"],"msg":"Field required","input":{}},` +
				`{"type":"missing","loc":["body","prompt_template"],"msg":"Field required","input":{}}]}`},
	} {
		code, _, raw := e.req("alice", "ws-alpha", "POST", pbBase, c.body)
		if code != 422 || !jsonEqual(raw, c.want) {
			t.Errorf("%s: %d %s", c.name, code, raw)
		}
	}
	if code, _, raw := e.req("alice", "ws-alpha", "POST", pbBase, ""); code != 422 ||
		!jsonEqual(raw, `{"detail":[{"type":"missing","loc":["body"],"msg":"Field required","input":null}]}`) {
		t.Errorf("no body: %d %s", code, raw)
	}
	if n := e.scalar(t, `SELECT count(*) FROM research_playbooks WHERE workspace_id='ws-alpha'`); n != 3 {
		t.Errorf("a rejected create wrote a row: %d playbooks", n)
	}
}

func TestPatchNullName422NotPanic(t *testing.T) {
	e := newPBEnv(t)
	code, _, raw := e.req("alice", "ws-alpha", "PATCH", pbBase+"/pb-gov-multi", `{"name":null}`)
	want := `{"detail":[{"type":"string_type","loc":["body","name"],"msg":"Input should be a valid string","input":null}]}`
	if code != 422 || !jsonEqual(raw, want) {
		t.Fatalf("%d %s", code, raw)
	}
	if n := e.text(t, `SELECT name FROM research_playbooks WHERE id='pb-gov-multi'`); n != "Multi step research" {
		t.Errorf("name changed to %q", n)
	}
	// every other NOT NULL column answers 422 too, never 500
	for _, f := range []string{"description", "prompt_template", "output_format", "max_steps", "cell_budget_usd", "enabled", "steps"} {
		if code, _, raw := e.req("alice", "ws-alpha", "PATCH", pbBase+"/pb-gov-multi", `{"`+f+`":null}`); code != 422 {
			t.Errorf("%s null: %d %s", f, code, raw)
		}
	}
}

func TestPatchNameCollision409(t *testing.T) {
	e := newPBEnv(t)
	code, _, raw := e.req("alice", "ws-alpha", "PATCH", pbBase+"/pb-gov-multi", `{"name":"Scheduled research"}`)
	if code != 409 || !jsonEqual(raw, `{"detail":"A playbook with this name already exists"}`) {
		t.Fatalf("%d %s", code, raw)
	}
	if n := e.text(t, `SELECT name FROM research_playbooks WHERE id='pb-gov-multi'`); n != "Multi step research" {
		t.Errorf("name changed to %q", n)
	}
	// renaming to its own name is not a collision
	if code, _, raw := e.req("alice", "ws-alpha", "PATCH", pbBase+"/pb-gov-multi", `{"name":"Multi step research"}`); code != 200 {
		t.Errorf("same name: %d %s", code, raw)
	}
	// the same name in another workspace is fine (fixture: pb_create_same_name_beta_201)
	if code, _, raw := e.req("bob", "ws-beta", "PATCH", pbBase+"/pb-gov-b", `{"name":"Scheduled research"}`); code != 200 {
		t.Errorf("other workspace: %d %s", code, raw)
	}
}

func pendingSchedule(e pbEnv, t *testing.T, pb string) int {
	return e.scalar(t, `SELECT count(*) FROM jobs WHERE type='research_playbook_schedule' AND status='pending' AND fire_key LIKE $1`, "playbook_schedule:"+pb+":%")
}

func TestPatchSchedulesNextRun(t *testing.T) {
	e := newPBEnv(t)
	if n := pendingSchedule(e, t, "pb-gov-off"); n != 0 {
		t.Fatalf("seed: %d pending", n)
	}
	before := time.Now().UTC()
	code, resp, raw := e.req("alice", "ws-alpha", "PATCH", pbBase+"/pb-gov-off", `{"enabled":true,"schedule_audience_id":"aud-gov-1","schedule_interval_minutes":60}`)
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	var pb struct {
		NextRunAt string `json:"next_run_at"`
	}
	_ = json.Unmarshal(resp.Body, &pb)
	next, err := time.Parse("2006-01-02T15:04:05.999999", pb.NextRunAt)
	if err != nil || next.Before(before.Add(59*time.Minute)) || next.After(time.Now().UTC().Add(61*time.Minute)) {
		t.Errorf("next_run_at %q", pb.NextRunAt)
	}
	if n := pendingSchedule(e, t, "pb-gov-off"); n != 1 {
		t.Errorf("pending schedule jobs %d, want 1", n)
	}
	if n := e.scalar(t, `SELECT count(*) FROM playbook_schedules WHERE playbook_id='pb-gov-off' AND enabled`); n != 1 {
		t.Errorf("mirror not enabled")
	}
	// re-patching replaces the pending occurrence instead of stacking another
	if code, _, raw := e.req("alice", "ws-alpha", "PATCH", pbBase+"/pb-gov-off", `{"schedule_interval_minutes":30}`); code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	if n := pendingSchedule(e, t, "pb-gov-off"); n != 1 {
		t.Errorf("pending schedule jobs after reschedule %d, want 1", n)
	}
	if n := e.scalar(t, `SELECT count(*) FROM jobs WHERE type='research_playbook_schedule' AND status='cancelled' AND fire_key LIKE 'playbook_schedule:pb-gov-off:%'`); n != 1 {
		t.Errorf("cancelled occurrences %d, want 1", n)
	}
	// the sibling playbook's occurrence is untouched
	if n := pendingSchedule(e, t, "pb-gov-sched"); n != 1 {
		t.Errorf("pb-gov-sched pending %d, want 1", n)
	}
}

func TestPatchDisableClearsSchedule(t *testing.T) {
	e := newPBEnv(t)
	code, resp, raw := e.req("alice", "ws-alpha", "PATCH", pbBase+"/pb-gov-sched", `{"enabled":false}`)
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	var pb struct {
		NextRunAt *string `json:"next_run_at"`
	}
	_ = json.Unmarshal(resp.Body, &pb)
	if pb.NextRunAt != nil {
		t.Errorf("next_run_at %q, want null", *pb.NextRunAt)
	}
	if n := pendingSchedule(e, t, "pb-gov-sched"); n != 0 {
		t.Errorf("pending schedule jobs %d, want 0", n)
	}
	if n := e.scalar(t, `SELECT count(*) FROM playbook_schedules WHERE playbook_id='pb-gov-sched' AND NOT enabled AND next_run_at IS NULL`); n != 1 {
		t.Errorf("mirror not cleared")
	}
}

func TestDeleteWithActiveRun409(t *testing.T) {
	e := newPBEnv(t)
	code, _, raw := e.req("alice", "ws-alpha", "DELETE", pbBase+"/pb-gov-sched", "")
	if code != 409 || !jsonEqual(raw, `{"detail":"Cannot delete a playbook with an active run"}`) {
		t.Fatalf("%d %s", code, raw)
	}
	if n := e.scalar(t, `SELECT count(*) FROM research_playbooks WHERE id='pb-gov-sched'`) +
		pendingSchedule(e, t, "pb-gov-sched"); n != 2 {
		t.Errorf("a refused delete removed state (%d)", n)
	}
	// one active run is enough, and finishing them all lets the delete through
	e.exec(t, `UPDATE playbook_runs SET status='completed' WHERE playbook_id='pb-gov-sched' AND id <> 'run-gov-r8'`)
	if code, _, _ := e.req("alice", "ws-alpha", "DELETE", pbBase+"/pb-gov-sched", ""); code != 409 {
		t.Errorf("one active run left: %d", code)
	}
	e.exec(t, `UPDATE playbook_runs SET status='failed' WHERE playbook_id='pb-gov-sched'`)
	if code, _, raw := e.req("alice", "ws-alpha", "DELETE", pbBase+"/pb-gov-sched", ""); code != 204 || raw != "" {
		t.Errorf("delete: %d %q", code, raw)
	}
}

func TestDeleteRemovesScheduleMirrorAndPendingJob(t *testing.T) {
	e := newPBEnv(t)
	e.exec(t, `UPDATE playbook_runs SET status='completed' WHERE playbook_id='pb-gov-sched'`)
	if code, _, raw := e.req("alice", "ws-alpha", "DELETE", pbBase+"/pb-gov-sched", ""); code != 204 {
		t.Fatalf("%d %s", code, raw)
	}
	if n := e.scalar(t, `SELECT count(*) FROM research_playbooks WHERE id='pb-gov-sched'`); n != 0 {
		t.Errorf("playbook row left")
	}
	if n := e.scalar(t, `SELECT count(*) FROM playbook_schedules WHERE playbook_id='pb-gov-sched'`); n != 0 {
		t.Errorf("mirror row left")
	}
	if n := pendingSchedule(e, t, "pb-gov-sched"); n != 0 {
		t.Errorf("pending schedule job left")
	}
	if n := e.scalar(t, `SELECT count(*) FROM jobs WHERE fire_key LIKE 'playbook_schedule:pb-gov-sched:%' AND status='cancelled'`); n != 1 {
		t.Errorf("schedule job not cancelled")
	}
	if n := e.scalar(t, `SELECT count(*) FROM playbook_runs WHERE playbook_id='pb-gov-sched'`); n != 0 {
		t.Errorf("runs did not cascade")
	}
}

func TestViewerCanListButNotWrite(t *testing.T) {
	e := newPBEnv(t)
	if code, _, raw := e.req("bob", "ws-alpha", "GET", pbBase, ""); code != 200 || !strings.Contains(raw, "pb-gov-multi") {
		t.Errorf("viewer list: %d %s", code, raw)
	}
	deny := `{"detail":"Workspace permission denied: agents.write"}`
	for _, c := range []struct{ m, p, b string }{
		{"POST", pbBase, `{"name":"x","prompt_template":"Describe {company} please."}`},
		{"PATCH", pbBase + "/pb-gov-multi", `{"name":"x"}`},
		{"DELETE", pbBase + "/pb-gov-off", ""},
		{"POST", pbBase, `{}`}, // the gate answers before body validation
	} {
		if code, _, raw := e.req("bob", "ws-alpha", c.m, c.p, c.b); code != 403 || !jsonEqual(raw, deny) {
			t.Errorf("%s %s: %d %s", c.m, c.p, code, raw)
		}
	}
	if n := e.scalar(t, `SELECT count(*) FROM research_playbooks WHERE workspace_id='ws-alpha'`); n != 3 {
		t.Errorf("a denied write changed the world: %d", n)
	}
	// an editor may write
	if code, _, raw := e.req("erin", "ws-alpha", "POST", pbBase, `{"name":"By erin","prompt_template":"Describe {company} please."}`); code != 201 {
		t.Errorf("editor create: %d %s", code, raw)
	}
}

func TestPlaybookTenantIsolation404(t *testing.T) {
	e := newPBEnv(t)
	notFound := `{"detail":"Playbook not found"}`
	for _, c := range []struct{ who, ws, m, p, b string }{
		{"alice", "ws-alpha", "PATCH", pbBase + "/pb-gov-b", `{"name":"x"}`},
		{"alice", "ws-alpha", "DELETE", pbBase + "/pb-gov-b", ""},
		{"bob", "ws-beta", "PATCH", pbBase + "/pb-gov-multi", `{"name":"x"}`},
		{"bob", "ws-beta", "DELETE", pbBase + "/pb-gov-multi", ""},
	} {
		if code, _, raw := e.req(c.who, c.ws, c.m, c.p, c.b); code != 404 || !jsonEqual(raw, notFound) {
			t.Errorf("%s %s as %s: %d %s", c.m, c.p, c.who, code, raw)
		}
	}
	// a foreign workspace's audience cannot be scheduled
	if code, _, raw := e.req("alice", "ws-alpha", "PATCH", pbBase+"/pb-gov-off", `{"schedule_audience_id":"aud-gov-b","schedule_interval_minutes":60}`); code != 404 ||
		!jsonEqual(raw, `{"detail":"Scheduled audience not found"}`) {
		t.Errorf("foreign audience: %d %s", code, raw)
	}
	// the empty id is not an audience either (Python 500s on the foreign key)
	if code, _, raw := e.req("alice", "ws-alpha", "PATCH", pbBase+"/pb-gov-off", `{"schedule_audience_id":"","schedule_interval_minutes":60}`); code != 404 {
		t.Errorf("empty audience: %d %s", code, raw)
	}
	if n := e.text(t, `SELECT name FROM research_playbooks WHERE id='pb-gov-b'`); n != "Beta research" {
		t.Errorf("foreign playbook changed: %q", n)
	}
	var rows []struct {
		ID string `json:"id"`
	}
	_, resp, _ := e.req("bob", "ws-beta", "GET", pbBase, "")
	_ = json.Unmarshal(resp.Body, &rows)
	if len(rows) != 1 || rows[0].ID != "pb-gov-b" {
		t.Errorf("beta list %v", rows)
	}
}

// A PATCH of steps keeps only the step keys the request sent (pydantic exclude_unset), so an omitted step
// output_format is stored and returned absent, while POST fills the default in.
func TestPatchStepsKeepOnlySentKeys(t *testing.T) {
	e := newPBEnv(t)
	code, _, raw := e.req("alice", "ws-alpha", "PATCH", pbBase+"/pb-gov-off", `{"steps":[{"key":"a","name":"A","prompt_template":"Step A for {company}."},{"key":"b","name":"B","prompt_template":"Step B for {company}.","output_format":"json"}]}`)
	want := `"steps":[{"key":"a","name":"A","prompt_template":"Step A for {company}."},{"key":"b","name":"B","prompt_template":"Step B for {company}.","output_format":"json"}]`
	if code != 200 || !strings.Contains(raw, want) {
		t.Fatalf("%d %s", code, raw)
	}
	_, _, raw = e.req("alice", "ws-alpha", "GET", pbBase, "")
	if !strings.Contains(raw, want) {
		t.Errorf("list: %s", raw)
	}
}
