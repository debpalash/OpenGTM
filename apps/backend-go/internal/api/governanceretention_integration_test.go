//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type grEnv struct {
	mcpEnv
	owner *pgxpool.Pool
	ids   map[string]int32
	cfg   *grCohort
}

// grCohort is the live executor routing: GO_EXECUTOR_ENABLED and GO_EXECUTOR_WORKSPACES, read per request.
type grCohort struct{ enabled, workspaces string }

// newGREnv seeds the governance fixture world for retention: alice owner of ws-alpha, bob a plain member of ws-alpha and
// owner of ws-beta, erin an editor of ws-alpha; ws-alpha holds the recorded policy, the three recorded runs and one expired
// row (2020-01-01) in every retention target. goExec turns the retention executor flag and cohort on for ws-alpha.
func newGREnv(t *testing.T) grEnv {
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
	cfg := &grCohort{}
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Flags: config.Flags{PGLeadStore: true}}
	d.Flags.Governance.GoExecutorRetention = true
	d.Getenv = func(k string) string {
		return map[string]string{"GO_EXECUTOR_ENABLED": cfg.enabled, "GO_EXECUTOR_WORKSPACES": cfg.workspaces}[k]
	}
	registerGovernanceRetention(r, d)
	e := grEnv{mcpEnv: mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}, owner: owner, ids: map[string]int32{}, cfg: cfg}
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

var grTables = []string{"governance_audit_events", "llm_usage_daily", "signals", "destination_deliveries",
	"destination_inbound_receipts", "audience_membership_events", "playbook_results", "outreach_sends",
	"retention_policies", "retention_runs"}

func (e grEnv) purge() {
	ctx := context.Background()
	c, err := e.owner.Acquire(ctx)
	if err != nil {
		return
	}
	defer c.Release()
	_, _ = c.Exec(ctx, `SET session_replication_role = replica`)
	for _, tb := range grTables {
		_, _ = c.Exec(ctx, `DELETE FROM `+tb+` WHERE workspace_id IN ('ws-alpha','ws-beta')`)
	}
	_, _ = c.Exec(ctx, `DELETE FROM retention_schedules WHERE workspace_id IN ('ws-alpha','ws-beta')`)
	_, _ = c.Exec(ctx, `DELETE FROM jobs WHERE type='retention_enforce' AND workspace_id IN ('ws-alpha','ws-beta') OR fire_key LIKE 'retention:ws-alpha:%' OR fire_key LIKE 'retention:ws-beta:%'`)
	_, _ = c.Exec(ctx, `RESET session_replication_role`)
}

func (e grEnv) exec(t *testing.T, sql string, args ...any) {
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

func (e grEnv) scalar(t *testing.T, sql string, args ...any) (n int) {
	t.Helper()
	if err := e.owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// seedWorld recreates the recorded seed: the alpha policy (30 days everywhere, audit 90), runs 1-3 and one expired row
// per retention target (activation has two tables, hence its count of 2).
func (e grEnv) seedWorld(t *testing.T) {
	t.Helper()
	days := `{"audit":90,"llm_usage":30,"signals":30,"activation":30,"audience_history":30,"agent_results":30,"outreach_history":30}`
	e.exec(t, `INSERT INTO retention_policies (workspace_id, enabled, legal_hold, retention_days, next_run_at, updated_by, created_at, updated_at)
		VALUES ('ws-alpha', true, false, $1::json, '2026-09-01 10:00:00', $2, '2026-08-20 08:00:00', '2026-08-27 08:00:00')`, days, e.ids["alice"])
	for _, r := range []struct{ id, by, counts, errText, started, finished, created, status string }{
		{"run-ret-1", "1", `{"audit":3,"signals":1}`, "", "2026-08-20 10:00:00", "2026-08-20 10:00:09", "2026-08-20 10:00:00", "completed"},
		{"run-ret-2", "scheduler", `{}`, "Final failure: worker killed", "2026-08-20 10:00:00", "2026-08-20 10:00:30", "2026-08-20 10:00:00", "failed"},
		{"run-ret-3", "4", `{}`, "Legal hold enabled during retention enforcement", "", "2026-08-27 08:01:00", "2026-08-27 08:00:00", "cancelled"},
	} {
		e.exec(t, `INSERT INTO retention_runs (id, workspace_id, status, requested_by, policy_snapshot, deleted_counts, error, started_at, finished_at, created_at)
			VALUES ($1,'ws-alpha',$8,$2,$3::json,$4::json,NULLIF($5,''),NULLIF($6,'')::timestamp,$7::timestamp,$9::timestamp)`,
			r.id, r.by, days, r.counts, r.errText, r.started, r.finished, r.status, r.created)
	}
	for _, ws := range []string{"ws-alpha", "ws-beta"} {
		e.seedExpired(t, ws)
	}
}

// seedExpired adds one row older than every category's minimum (2020-01-01) to each retention target of ws.
func (e grEnv) seedExpired(t *testing.T, ws string) {
	t.Helper()
	old := "2020-01-01 00:00:00"
	e.exec(t, `INSERT INTO governance_audit_events (id,workspace_id,actor_role,method,route,resource_path,response_status,outcome,request_id,metadata_json,created_at) VALUES ('gr-old-'||$1::text,$1::text,'admin','POST','/r','/p',200,'success','rid','{}',$2::timestamp)`, ws, old)
	e.exec(t, `INSERT INTO llm_usage_daily (workspace_id,provider,date,updated_at) VALUES ($1::text,'p','2020-01-01','x')`, ws)
	e.exec(t, `INSERT INTO signals (id,workspace_id,created_at) VALUES ('gr-old-s-'||$1::text,$1::text,1577836800)`, ws)
	e.exec(t, `INSERT INTO destination_deliveries (workspace_id,run_id,destination_id,lead_id,operation,idempotency_key,payload_fingerprint,status,attempts,created_at) VALUES ($1::text,'r','d',1,'upsert','k-'||$1::text,'f','sent',0,$2::timestamp)`, ws, old)
	e.exec(t, `INSERT INTO destination_inbound_receipts (id,workspace_id,destination_id,provider,external_event_id,status,conflict_policy,applied_fields,ignored_fields,payload_fingerprint,created_at) VALUES ('gr-old-i-'||$1::text,$1::text,'d','p','e1','ok','x','{}','{}','f',$2::timestamp)`, ws, old)
	e.exec(t, `INSERT INTO audience_membership_events (workspace_id,audience_id,lead_id,event_type,snapshot,created_at) VALUES ($1::text,'a',1,'enter','{}',$2::timestamp)`, ws, old)
	e.exec(t, `INSERT INTO playbook_results (workspace_id,run_id,lead_id,status,value,result_metadata,attempts,created_at) VALUES ($1::text,'r',1,'ok','v','{}',0,$2::timestamp)`, ws, old)
	e.exec(t, `INSERT INTO outreach_sends (workspace_id,to_email,idempotency_key,created_at) VALUES ($1::text,'a@b.c','k-'||$1::text,$2::timestamptz)`, ws, old)
}

func (e grEnv) req(who, ws, method, path, body string) (int, contract.Resp, string) {
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

// grCases is the recorded order of the retention cases (the state they build on is part of the contract), with the JSON
// paths the recording marks volatile. updated_by and requested_by hold the seeded user ids, which differ per database.
var grCases = []struct {
	name, who, ws string
	volatile      []string
}{
	{"ret_get_alpha_policy", "alice", "ws-alpha", nil},
	{"ret_get_beta_no_policy", "bob", "ws-beta", nil},
	{"ret_preview_alpha", "alice", "ws-alpha", nil},
	{"ret_preview_beta_defaults", "bob", "ws-beta", nil},
	{"ret_runs_default", "alice", "ws-alpha", nil},
	{"ret_runs_beta_empty", "bob", "ws-beta", nil},
	{"ret_runs_limit1", "alice", "ws-alpha", nil},
	{"ret_runs_limit1_cursor", "alice", "ws-alpha", nil},
	{"ret_runs_limit1_cursor2", "alice", "ws-alpha", nil},
	{"ret_runs_offset1", "alice", "ws-alpha", nil},
	{"ret_runs_offset_ignored_with_cursor", "alice", "ws-alpha", nil},
	{"ret_runs_offset_past_end", "alice", "ws-alpha", nil},
	{"ret_runs_limit_0_422", "alice", "ws-alpha", nil},
	{"ret_runs_limit_201_422", "alice", "ws-alpha", nil},
	{"ret_runs_offset_100001_422", "alice", "ws-alpha", nil},
	{"ret_runs_cursor_forged_422", "alice", "ws-alpha", nil},
	{"ret_runs_cursor_naive_time", "alice", "ws-alpha", nil},
	{"ret_get_bob_403", "bob", "ws-alpha", nil},
	{"ret_preview_bob_403", "bob", "ws-alpha", nil},
	{"ret_runs_bob_403", "bob", "ws-alpha", nil},
	{"ret_put_bob_403", "bob", "ws-alpha", nil},
	{"ret_put_erin_403", "erin", "ws-alpha", nil},
	{"ret_enforce_erin_403", "erin", "ws-alpha", nil},
	{"ret_enforce_wrong_confirmation_422", "alice", "ws-alpha", nil},
	{"ret_enforce_missing_body_422", "alice", "ws-alpha", nil},
	{"ret_enforce_no_policy_409", "bob", "ws-beta", nil},
	{"ret_put_missing_enabled_422", "alice", "ws-alpha", nil},
	{"ret_put_min_violation_422", "alice", "ws-alpha", nil},
	{"ret_put_over_max_422", "alice", "ws-alpha", nil},
	{"ret_put_unknown_category_422", "alice", "ws-alpha", nil},
	{"ret_put_non_integer_422", "alice", "ws-alpha", nil},
	{"ret_put_float_days_422", "alice", "ws-alpha", nil},
	{"ret_put_min_boundary", "alice", "ws-alpha", []string{"$.next_run_at", "$.updated_at"}},
	{"ret_put_max_boundary", "alice", "ws-alpha", []string{"$.next_run_at", "$.updated_at"}},
	{"ret_put_disabled", "alice", "ws-alpha", []string{"$.updated_at"}},
	{"ret_put_valid", "alice", "ws-alpha", []string{"$.next_run_at", "$.updated_at"}},
	{"ret_preview_after_put", "alice", "ws-alpha", nil},
	{"ret_put_legal_hold", "alice", "ws-alpha", []string{"$.updated_at"}},
	{"ret_preview_legal_hold", "alice", "ws-alpha", nil},
	{"ret_enforce_legal_hold_409", "alice", "ws-alpha", nil},
	{"ret_put_clear_hold", "alice", "ws-alpha", []string{"$.next_run_at", "$.updated_at"}},
	{"ret_enforce_success_202", "alice", "ws-alpha", []string{"$.created_at", "$.id", "$.requested_by"}},
	{"ret_runs_after_enforce", "alice", "ws-alpha", []string{"$.runs[0].created_at", "$.runs[0].id", "$.runs[0].requested_by"}},
	{"ret_put_beta_creates_policy", "bob", "ws-beta", []string{"$.created_at", "$.updated_at"}},
	{"ret_get_beta_after_put", "bob", "ws-beta", []string{"$.created_at", "$.updated_at"}},
	{"ret_put_bool_strings", "alice", "ws-alpha", []string{"$.next_run_at", "$.updated_at"}},
}

func TestRetentionRoutesMatchFixtures(t *testing.T) {
	e := newGREnv(t)
	for _, c := range grCases {
		fx := contract.Load(t, "retention/"+c.name)
		body := ""
		if len(fx.Request.Body) > 0 && string(fx.Request.Body) != "null" {
			body = string(fx.Request.Body)
		}
		_, got, _ := e.req(c.who, c.ws, fx.Request.Method, fx.Request.Path, body)
		shape := append([]string{"$.updated_by"}, c.volatile...)
		t.Run(c.name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response, contract.IgnoreShapeOf(shape...)) })
	}
}

func TestRetentionActorIdsAreTheCaller(t *testing.T) {
	e := newGREnv(t)
	_, resp, _ := e.req("alice", "ws-alpha", "PUT", "/api/governance/retention", `{"enabled":true}`)
	var p struct {
		UpdatedBy int32 `json:"updated_by"`
	}
	_ = json.Unmarshal(resp.Body, &p)
	if p.UpdatedBy != e.ids["alice"] {
		t.Errorf("updated_by %d, want %d", p.UpdatedBy, e.ids["alice"])
	}
	_, resp, _ = e.req("alice", "ws-alpha", "POST", "/api/governance/retention/enforce", `{"confirmation":"PURGE EXPIRED DATA"}`)
	var run struct {
		RequestedBy string `json:"requested_by"`
	}
	_ = json.Unmarshal(resp.Body, &run)
	if run.RequestedBy != strconv.Itoa(int(e.ids["alice"])) {
		t.Errorf("requested_by %q", run.RequestedBy)
	}
}

func TestRetentionPutInvalidDays422(t *testing.T) {
	e := newGREnv(t)
	for _, c := range []struct{ name, body, detail string }{
		{"below min", `{"enabled":true,"retention_days":{"audit":89}}`, "audit retention must be 90..3650 days"},
		{"llm below min", `{"enabled":true,"retention_days":{"llm_usage":29}}`, "llm_usage retention must be 30..3650 days"},
		{"over max", `{"enabled":true,"retention_days":{"signals":3651}}`, "signals retention must be 30..3650 days"},
		{"negative", `{"enabled":true,"retention_days":{"signals":-5}}`, "signals retention must be 30..3650 days"},
		{"huge", `{"enabled":true,"retention_days":{"signals":99999999999999999999999}}`, "signals retention must be 30..3650 days"},
		{"unknown sorted", `{"enabled":true,"retention_days":{"zeta":100,"bogus":100}}`, "unsupported retention categories: bogus, zeta"},
		// Python reports the first offending key in REQUEST order, not category order.
		{"request order", `{"enabled":true,"retention_days":{"signals":5,"audit":5}}`, "signals retention must be 30..3650 days"},
		{"request order 2", `{"enabled":true,"retention_days":{"audit":5,"signals":5}}`, "audit retention must be 90..3650 days"},
		{"whole float below min", `{"enabled":true,"retention_days":{"audit":10.0}}`, "audit retention must be 90..3650 days"},
		{"bool true is 1", `{"enabled":true,"retention_days":{"audit":true}}`, "audit retention must be 90..3650 days"},
	} {
		code, _, raw := e.req("alice", "ws-alpha", "PUT", "/api/governance/retention", c.body)
		var got struct {
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal([]byte(raw), &got); code != 422 || err != nil || got.Detail != c.detail {
			t.Errorf("%s: %d %s", c.name, code, raw)
		}
	}
	// nothing was written by any rejected request
	if p := e.scalar(t, `SELECT count(*) FROM retention_policies WHERE workspace_id='ws-alpha' AND updated_at <> '2026-08-27 08:00:00'`); p != 0 {
		t.Errorf("a rejected PUT changed the policy")
	}
}

func TestRetentionPutMissingEnabled422Body(t *testing.T) {
	e := newGREnv(t)
	for _, c := range []struct{ name, body, want string }{
		{"missing enabled", `{"retention_days":{"audit":100}}`,
			`{"detail":[{"type":"missing","loc":["body","enabled"],"msg":"Field required","input":{"retention_days":{"audit":100}}}]}`},
		{"empty object", `{}`, `{"detail":[{"type":"missing","loc":["body","enabled"],"msg":"Field required","input":{}}]}`},
		{"enabled null", `{"enabled":null}`, `{"detail":[{"type":"bool_type","loc":["body","enabled"],"msg":"Input should be a valid boolean","input":null}]}`},
		{"enabled word", `{"enabled":"maybe"}`, `{"detail":[{"type":"bool_parsing","loc":["body","enabled"],"msg":"Input should be a valid boolean, unable to interpret input","input":"maybe"}]}`},
		{"days list", `{"enabled":true,"retention_days":[1]}`, `{"detail":[{"type":"dict_type","loc":["body","retention_days"],"msg":"Input should be a valid dictionary","input":[1]}]}`},
		{"days null", `{"enabled":true,"retention_days":null}`, `{"detail":[{"type":"dict_type","loc":["body","retention_days"],"msg":"Input should be a valid dictionary","input":null}]}`},
		{"days null value", `{"enabled":true,"retention_days":{"audit":null}}`, `{"detail":[{"type":"int_type","loc":["body","retention_days","audit"],"msg":"Input should be a valid integer","input":null}]}`},
		{"array body", `[1]`, `{"detail":[{"type":"model_attributes_type","loc":["body"],"msg":"Input should be a valid dictionary or object to extract fields from","input":[1]}]}`},
		{"two errors in field order", `{"legal_hold":"x","retention_days":{"audit":"abc"}}`,
			`{"detail":[{"type":"missing","loc":["body","enabled"],"msg":"Field required","input":{"legal_hold":"x","retention_days":{"audit":"abc"}}},` +
				`{"type":"bool_parsing","loc":["body","legal_hold"],"msg":"Input should be a valid boolean, unable to interpret input","input":"x"},` +
				`{"type":"int_parsing","loc":["body","retention_days","audit"],"msg":"Input should be a valid integer, unable to parse string as an integer","input":"abc"}]}`},
	} {
		code, _, raw := e.req("alice", "ws-alpha", "PUT", "/api/governance/retention", c.body)
		if code != 422 || !jsonEqual(raw, c.want) {
			t.Errorf("%s: %d %s", c.name, code, raw)
		}
	}
	if code, _, raw := e.req("alice", "ws-alpha", "PUT", "/api/governance/retention", ""); code != 422 ||
		!jsonEqual(raw, `{"detail":[{"type":"missing","loc":["body"],"msg":"Field required","input":null}]}`) {
		t.Errorf("no body: %d %s", code, raw)
	}
}

func jsonEqual(a, b string) bool {
	var x, y any
	return json.Unmarshal([]byte(a), &x) == nil && json.Unmarshal([]byte(b), &y) == nil && fmt.Sprint(x) == fmt.Sprint(y)
}

func TestRetentionPutReschedulesOnHoldAndEnableChange(t *testing.T) {
	e := newGREnv(t)
	pending := func() int {
		return e.scalar(t, `SELECT count(*) FROM jobs WHERE type='retention_enforce' AND status='pending' AND fire_key LIKE 'retention:ws-alpha:%'`)
	}
	put := func(body string) {
		t.Helper()
		if code, _, raw := e.req("alice", "ws-alpha", "PUT", "/api/governance/retention", body); code != 200 {
			t.Fatalf("%s: %d %s", body, code, raw)
		}
	}
	put(`{"enabled":true}`)
	if pending() != 1 || e.scalar(t, `SELECT count(*) FROM retention_schedules WHERE workspace_id='ws-alpha' AND enabled AND next_run_at IS NOT NULL`) != 1 {
		t.Fatal("enable: one pending job and an enabled mirror expected")
	}
	put(`{"enabled":true,"legal_hold":true}`)
	if pending() != 0 || e.scalar(t, `SELECT count(*) FROM retention_schedules WHERE workspace_id='ws-alpha' AND NOT enabled AND next_run_at IS NULL`) != 1 {
		t.Fatal("hold set: no pending job and a disabled mirror expected")
	}
	put(`{"enabled":true,"legal_hold":false}`)
	if pending() != 1 {
		t.Fatal("hold cleared: a pending job expected")
	}
	put(`{"enabled":false}`)
	if pending() != 0 || e.scalar(t, `SELECT count(*) FROM retention_policies WHERE workspace_id='ws-alpha' AND next_run_at IS NULL AND NOT enabled`) != 1 {
		t.Fatal("disabled: no pending job and no next run expected")
	}
	// another workspace's schedule is not touched
	e.req("bob", "ws-beta", "PUT", "/api/governance/retention", `{"enabled":true}`)
	put(`{"enabled":false}`)
	if e.scalar(t, `SELECT count(*) FROM jobs WHERE status='pending' AND fire_key LIKE 'retention:ws-beta:%'`) != 1 {
		t.Fatal("beta's job must survive alpha's update")
	}
}

func (e grEnv) enforce(who, ws, confirmation string) (int, string) {
	b, _ := json.Marshal(map[string]string{"confirmation": confirmation})
	code, _, raw := e.req(who, ws, "POST", "/api/governance/retention/enforce", string(b))
	return code, raw
}

func (e grEnv) runsAndJobs(t *testing.T) (runs, jobs int) {
	t.Helper()
	return e.scalar(t, `SELECT count(*) FROM retention_runs`),
		e.scalar(t, `SELECT count(*) FROM jobs WHERE type='retention_enforce' AND fire_key LIKE 'retention-run:%'`)
}

func TestEnforceRequiresExactConfirmation(t *testing.T) {
	e := newGREnv(t)
	r0, j0 := e.runsAndJobs(t)
	for _, c := range []string{"purge expired data", "PURGE EXPIRED DATA ", " PURGE EXPIRED DATA", "PURGE", ""} {
		code, raw := e.enforce("alice", "ws-alpha", c)
		if code != 422 || !jsonEqual(raw, `{"detail":"confirmation must equal PURGE EXPIRED DATA"}`) {
			t.Errorf("%q: %d %s", c, code, raw)
		}
	}
	for _, c := range []struct{ body, want string }{
		{`{"confirmation":5}`, `{"detail":[{"type":"string_type","loc":["body","confirmation"],"msg":"Input should be a valid string","input":5}]}`},
		{`{"confirmation":null}`, `{"detail":[{"type":"string_type","loc":["body","confirmation"],"msg":"Input should be a valid string","input":null}]}`},
	} {
		if code, _, raw := e.req("alice", "ws-alpha", "POST", "/api/governance/retention/enforce", c.body); code != 422 || !jsonEqual(raw, c.want) {
			t.Errorf("%s: %d %s", c.body, code, raw)
		}
	}
	if r, j := e.runsAndJobs(t); r != r0 || j != j0 {
		t.Errorf("a rejected enforce created %d runs and %d jobs", r-r0, j-j0)
	}
	if code, raw := e.enforce("alice", "ws-alpha", "PURGE EXPIRED DATA"); code != 202 {
		t.Errorf("exact phrase: %d %s", code, raw)
	}
}

func TestEnforceNoPolicy409(t *testing.T) {
	e := newGREnv(t)
	r0, j0 := e.runsAndJobs(t)
	code, raw := e.enforce("bob", "ws-beta", "PURGE EXPIRED DATA")
	if code != 409 || !jsonEqual(raw, `{"detail":"Configure a retention policy first"}`) {
		t.Fatalf("%d %s", code, raw)
	}
	if r, j := e.runsAndJobs(t); r != r0 || j != j0 {
		t.Errorf("created %d runs, %d jobs", r-r0, j-j0)
	}
}

func TestEnforceLegalHold409(t *testing.T) {
	e := newGREnv(t)
	e.req("alice", "ws-alpha", "PUT", "/api/governance/retention", `{"enabled":true,"legal_hold":true}`)
	r0, j0 := e.runsAndJobs(t)
	code, raw := e.enforce("alice", "ws-alpha", "PURGE EXPIRED DATA")
	if code != 409 || !jsonEqual(raw, `{"detail":"Legal hold blocks retention enforcement"}`) {
		t.Fatalf("%d %s", code, raw)
	}
	if r, j := e.runsAndJobs(t); r != r0 || j != j0 {
		t.Errorf("created %d runs, %d jobs", r-r0, j-j0)
	}
}

func TestEnforceCreatesRunAndJobAtomically(t *testing.T) {
	e := newGREnv(t)
	code, raw := e.enforce("alice", "ws-alpha", "PURGE EXPIRED DATA")
	if code != 202 {
		t.Fatalf("%d %s", code, raw)
	}
	var run struct {
		ID             string
		Status         string
		RequestedBy    string         `json:"requested_by"`
		PolicySnapshot map[string]int `json:"policy_snapshot"`
	}
	if err := json.Unmarshal([]byte(raw), &run); err != nil {
		t.Fatal(err)
	}
	if run.Status != "pending" || run.RequestedBy != strconv.Itoa(int(e.ids["alice"])) || run.PolicySnapshot["audit"] != 90 || run.PolicySnapshot["signals"] != 30 {
		t.Errorf("run %s", raw)
	}
	if n := e.scalar(t, `SELECT count(*) FROM retention_runs WHERE id=$1 AND workspace_id='ws-alpha' AND status='pending'`, run.ID); n != 1 {
		t.Errorf("run rows %d", n)
	}
	var executor, status, fireKey, ws, payload string
	var offset float64
	if err := e.owner.QueryRow(context.Background(), `SELECT executor, status, fire_key, workspace_id, payload::text, extract(epoch from (next_run_at - created_at))
		FROM jobs WHERE type='retention_enforce' AND fire_key=$1`, "retention-run:"+run.ID).Scan(&executor, &status, &fireKey, &ws, &payload, &offset); err != nil {
		t.Fatalf("job: %v", err)
	}
	if executor != "go" || status != "pending" || ws != "ws-alpha" || offset != 0 || !jsonEqual(payload, fmt.Sprintf(`{"workspace_id":"ws-alpha","run_id":%q}`, run.ID)) {
		t.Errorf("job %s %s %s %v %s", executor, status, ws, offset, payload)
	}
}

func TestEnforceEnqueueFailureLeavesNoRun(t *testing.T) {
	e := newGREnv(t)
	e.exec(t, `CREATE OR REPLACE FUNCTION gr_fail_enqueue() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.fire_key LIKE 'retention-run:%' THEN RAISE EXCEPTION 'enqueue refused'; END IF; RETURN NEW; END $$`)
	e.exec(t, `CREATE TRIGGER gr_fail_enqueue BEFORE INSERT ON jobs FOR EACH ROW EXECUTE FUNCTION gr_fail_enqueue()`)
	t.Cleanup(func() {
		_, _ = e.owner.Exec(context.Background(), `DROP TRIGGER IF EXISTS gr_fail_enqueue ON jobs`)
		_, _ = e.owner.Exec(context.Background(), `DROP FUNCTION IF EXISTS gr_fail_enqueue()`)
	})
	r0, j0 := e.runsAndJobs(t)
	code, raw := e.enforce("alice", "ws-alpha", "PURGE EXPIRED DATA")
	if code != 500 || !jsonEqual(raw, `{"detail":"Internal Server Error"}`) {
		t.Fatalf("%d %s", code, raw)
	}
	if r, j := e.runsAndJobs(t); r != r0 || j != j0 {
		t.Errorf("failed enqueue left %d runs, %d jobs", r-r0, j-j0)
	}
}

func TestEnforceJobExecutorGoIgnoringRetiredFlag(t *testing.T) {
	e := newGREnv(t)
	e.exec(t, `INSERT INTO retention_policies (workspace_id, enabled, legal_hold, retention_days, updated_by) VALUES ('ws-beta', true, false, '{}', NULL)`)
	executorOf := func(ws, who string) string {
		t.Helper()
		// F19: an open run blocks the next enforce; finish the previous ones first.
		e.exec(t, `UPDATE retention_runs SET status='completed' WHERE workspace_id=$1 AND status IN ('pending','running')`, ws)
		code, raw := e.enforce(who, ws, "PURGE EXPIRED DATA")
		var run struct{ ID string }
		_ = json.Unmarshal([]byte(raw), &run)
		var got string
		if err := e.owner.QueryRow(context.Background(), `SELECT executor FROM jobs WHERE fire_key=$1`, "retention-run:"+run.ID).Scan(&got); code != 202 || err != nil {
			t.Fatalf("%s: %d %s %v", ws, code, raw, err)
		}
		return got
	}
	scheduledExecutor := func() string {
		t.Helper()
		e.req("alice", "ws-alpha", "PUT", "/api/governance/retention", `{"enabled":true}`)
		var got string
		if err := e.owner.QueryRow(context.Background(), `SELECT executor FROM jobs WHERE status='pending' AND fire_key LIKE 'retention:ws-alpha:%'`).Scan(&got); err != nil {
			t.Fatalf("scheduled job: %v", err)
		}
		return got
	}
	for _, c := range []struct {
		name, enabled, workspaces, alpha, beta string
	}{
		{"flag off", "", "ws-alpha", "go", "go"},
		{"in cohort", "true", "ws-alpha", "go", "go"},
		{"cohort of beta only", "true", "ws-beta", "go", "go"},
		{"empty cohort", "true", "", "go", "go"},
	} {
		e.cfg.enabled, e.cfg.workspaces = c.enabled, c.workspaces
		if got := executorOf("ws-alpha", "alice"); got != c.alpha {
			t.Errorf("%s: alpha enforce job %q, want %q", c.name, got, c.alpha)
		}
		if got := executorOf("ws-beta", "bob"); got != c.beta {
			t.Errorf("%s: beta enforce job %q, want %q", c.name, got, c.beta)
		}
		// the scheduled occurrence follows the same routing
		if got := scheduledExecutor(); got != c.alpha {
			t.Errorf("%s: alpha scheduled job %q, want %q", c.name, got, c.alpha)
		}
	}
}

func TestRunsPaginationOffsetCursor(t *testing.T) {
	e := newGREnv(t)
	type page struct {
		Runs       []struct{ ID string }
		Limit      int
		Offset     *int
		HasMore    bool    `json:"has_more"`
		NextCursor *string `json:"next_cursor"`
	}
	get := func(q string) page {
		t.Helper()
		code, _, raw := e.req("alice", "ws-alpha", "GET", "/api/governance/retention/runs"+q, "")
		var p page
		if code != 200 || json.Unmarshal([]byte(raw), &p) != nil {
			t.Fatalf("%s: %d %s", q, code, raw)
		}
		return p
	}
	ids := func(p page) string {
		var s []string
		for _, r := range p.Runs {
			s = append(s, r.ID)
		}
		return strings.Join(s, ",")
	}
	// created_at ties (run-ret-1 and run-ret-2) break by id descending
	if p := get(""); ids(p) != "run-ret-3,run-ret-2,run-ret-1" || p.Limit != 20 || p.Offset == nil || *p.Offset != 0 || p.HasMore || p.NextCursor != nil {
		t.Errorf("default: %+v", p)
	}
	if p := get("?limit=1&offset=1"); ids(p) != "run-ret-2" || !p.HasMore || p.NextCursor == nil || *p.Offset != 1 {
		t.Errorf("offset: %+v", p)
	}
	p1 := get("?limit=1")
	p2 := get("?limit=1&cursor=" + *p1.NextCursor)
	p3 := get("?limit=1&cursor=" + *p2.NextCursor)
	if ids(p1) != "run-ret-3" || ids(p2) != "run-ret-2" || ids(p3) != "run-ret-1" || p3.HasMore || p3.NextCursor != nil || p2.Offset != nil {
		t.Errorf("cursor chain: %s %s %s %+v", ids(p1), ids(p2), ids(p3), p2)
	}
	if p := get("?limit=1&offset=2&cursor=" + *p1.NextCursor); ids(p) != "run-ret-2" || p.Offset != nil {
		t.Errorf("offset with cursor: %+v", p)
	}
	if p := get("?offset=50"); len(p.Runs) != 0 || p.HasMore {
		t.Errorf("past end: %+v", p)
	}
	for _, q := range []string{"limit=0", "limit=201", "limit=x", "offset=-1", "offset=100001", "cursor=" + strings.Repeat("a", 1025)} {
		if code, _, raw := e.req("alice", "ws-alpha", "GET", "/api/governance/retention/runs?"+q, ""); code != 422 {
			t.Errorf("%s: %d %s", q, code, raw)
		}
	}
	for _, q := range []string{"limit=200", "offset=100000", "limit=+5", "limit=5.0"} {
		if code, _, raw := e.req("alice", "ws-alpha", "GET", "/api/governance/retention/runs?"+q, ""); code != 200 {
			t.Errorf("%s: %d %s", q, code, raw)
		}
	}
	// declaration order of the errors: limit, offset, cursor
	_, _, raw := e.req("alice", "ws-alpha", "GET", "/api/governance/retention/runs?cursor="+strings.Repeat("a", 1025)+"&offset=-1&limit=0", "")
	var body struct{ Detail []struct{ Loc []string } }
	_ = json.Unmarshal([]byte(raw), &body)
	if len(body.Detail) != 3 || body.Detail[0].Loc[1] != "limit" || body.Detail[1].Loc[1] != "offset" || body.Detail[2].Loc[1] != "cursor" {
		t.Errorf("error order: %s", raw)
	}
}

func TestRunsTenantIsolation(t *testing.T) {
	e := newGREnv(t)
	_, _, raw := e.req("bob", "ws-beta", "GET", "/api/governance/retention/runs", "")
	if !jsonEqual(raw, `{"runs":[],"limit":20,"offset":0,"has_more":false,"next_cursor":null}`) {
		t.Errorf("beta sees %s", raw)
	}
	// a cursor from alpha's list cannot leak alpha's rows into beta
	_, _, a := e.req("alice", "ws-alpha", "GET", "/api/governance/retention/runs?limit=1", "")
	var p struct {
		NextCursor string `json:"next_cursor"`
	}
	_ = json.Unmarshal([]byte(a), &p)
	_, _, raw = e.req("bob", "ws-beta", "GET", "/api/governance/retention/runs?cursor="+p.NextCursor, "")
	if strings.Contains(raw, "run-ret") {
		t.Errorf("leak: %s", raw)
	}
	// and beta's enforce/put never touch alpha
	e.req("bob", "ws-beta", "PUT", "/api/governance/retention", `{"enabled":false}`)
	if e.scalar(t, `SELECT count(*) FROM retention_policies WHERE workspace_id='ws-alpha' AND updated_at='2026-08-27 08:00:00'`) != 1 {
		t.Error("alpha's policy changed")
	}
}

func TestRetentionRoutesRequireAdmin(t *testing.T) {
	e := newGREnv(t)
	routes := []struct{ method, path, body string }{
		{"GET", "/api/governance/retention", ""},
		{"PUT", "/api/governance/retention", `{"enabled":false}`},
		{"POST", "/api/governance/retention/preview", ""},
		{"POST", "/api/governance/retention/enforce", `{"confirmation":"PURGE EXPIRED DATA"}`},
		{"GET", "/api/governance/retention/runs", ""},
	}
	r0, j0 := e.runsAndJobs(t)
	for _, rt := range routes {
		for _, who := range []string{"erin", "bob"} {
			if code, _, raw := e.req(who, "ws-alpha", rt.method, rt.path, rt.body); code != 403 {
				t.Errorf("%s %s %s: %d %s", who, rt.method, rt.path, code, raw)
			}
		}
		if code, _, _ := e.req("", "", rt.method, rt.path, rt.body); code != 401 {
			t.Errorf("anon %s %s: %d", rt.method, rt.path, code)
		}
	}
	if r, j := e.runsAndJobs(t); r != r0 || j != j0 {
		t.Errorf("a refused enforce created %d runs, %d jobs", r-r0, j-j0)
	}
	if e.scalar(t, `SELECT count(*) FROM retention_policies WHERE workspace_id='ws-alpha' AND NOT enabled`) != 0 {
		t.Error("a refused PUT changed the policy")
	}
}

// F19: concurrent enforces of one workspace give one 202 and one 409; a pending or running run
// (manual or scheduled) refuses a new one until it ends.
func TestEnforceConcurrentOneRun(t *testing.T) {
	e := newGREnv(t)
	r0, j0 := e.runsAndJobs(t)
	codes := make(chan int, 2)
	bodies := make(chan string, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, raw := e.enforce("alice", "ws-alpha", "PURGE EXPIRED DATA")
			codes <- code
			bodies <- raw
		}()
	}
	wg.Wait()
	close(codes)
	close(bodies)
	got := map[int]int{}
	for c := range codes {
		got[c]++
	}
	if got[202] != 1 || got[409] != 1 {
		t.Fatalf("codes %v", got)
	}
	found := false
	for b := range bodies {
		found = found || jsonEqual(b, `{"detail":"Retention enforcement already running"}`)
	}
	if !found {
		t.Fatal("no 409 detail")
	}
	if r, j := e.runsAndJobs(t); r != r0+1 || j != j0+1 {
		t.Errorf("created %d runs, %d jobs", r-r0, j-j0)
	}
	// a running scheduled run also blocks; a finished one does not.
	e.exec(t, `UPDATE retention_runs SET status='running', requested_by='scheduler' WHERE workspace_id='ws-alpha' AND status='pending'`)
	if code, raw := e.enforce("alice", "ws-alpha", "PURGE EXPIRED DATA"); code != 409 {
		t.Fatalf("running scheduled run: %d %s", code, raw)
	}
	e.exec(t, `UPDATE retention_runs SET status='failed' WHERE workspace_id='ws-alpha' AND status='running'`)
	if code, raw := e.enforce("alice", "ws-alpha", "PURGE EXPIRED DATA"); code != 202 {
		t.Fatalf("after failed run: %d %s", code, raw)
	}
}
