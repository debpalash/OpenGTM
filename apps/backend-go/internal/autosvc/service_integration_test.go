//go:build integration

package autosvc_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autosvc"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const (
	wsA = "ws-p08t13-a"
	wsB = "ws-p08t13-b"
)

var fixedNow = time.Date(2026, 8, 29, 9, 30, 0, 123456000, time.UTC)

type env struct {
	pool *pgxpool.Pool
	ctx  context.Context
	svc  *autosvc.Service
}

func inWS(t *testing.T, e *env, ws string, fn func(tx pgx.Tx) error) {
	t.Helper()
	if err := tenancy.InWorkspace(e.ctx, e.pool, ws, fn); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testpg.NewAppPool(t, 12)
	ctx := context.Background()
	e := &env{pool: pool, ctx: ctx}
	for _, ws := range []string{wsA, wsB} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, slug) VALUES ($1::varchar,$1::text,$1::text) ON CONFLICT DO NOTHING`, ws); err != nil {
			t.Fatal(err)
		}
	}
	clean := func() {
		for _, ws := range []string{wsA, wsB} {
			_ = tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
				for _, q := range []string{
					`DELETE FROM trigger_action_results`, `DELETE FROM trigger_runs`, `DELETE FROM triggers`,
					`DELETE FROM workbook_rows`, `DELETE FROM workbooks`, `DELETE FROM workspace_settings`,
				} {
					_, _ = tx.Exec(ctx, q)
				}
				_, _ = tx.Exec(ctx, `DELETE FROM jobs WHERE workspace_id = $1`, ws)
				_, _ = tx.Exec(ctx, `DELETE FROM scheduled_triggers WHERE workspace_id = $1`, ws)
				return nil
			})
		}
	}
	clean()
	t.Cleanup(clean)
	e.svc = &autosvc.Service{Pool: pool, Now: func() time.Time { return fixedNow },
		ExecFor: func(ws string) string { return "go" },
		Cfg: autosvc.Config{MaxRulesPerWS: 50, MaxActionsPerRule: 10,
			Secrets: secrets.NewResolver(nil, secrets.NewPgStore(pool), os.Getenv)}}
	return e
}

func ruleJSON(t *testing.T, tt string, cfg, actions, scope string) autosvc.RuleInput {
	t.Helper()
	in := autosvc.RuleInput{Name: "r", TriggerType: &tt, Enabled: true}
	if cfg != "" {
		c, err := autosvc.ParseJSON([]byte(cfg))
		if err != nil {
			t.Fatal(err)
		}
		in.TriggerConfig = c
	}
	if actions != "" {
		a, err := autosvc.ParseJSON([]byte(actions))
		if err != nil {
			t.Fatal(err)
		}
		in.Actions = a.([]any)
	}
	if scope != "" {
		if err := json.Unmarshal([]byte(scope), &in.ScopeWorkbookIDs); err != nil {
			t.Fatal(err)
		}
	}
	return in
}

func seedWorkbook(t *testing.T, e *env, ws, id string, cols string) {
	t.Helper()
	inWS(t, e, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO workbooks (id, name, workspace_id, columns_config) VALUES ($1,$1,$2,$3::json)`, id, ws, cols)
		return err
	})
}

func seedRow(t *testing.T, e *env, ws, wb string) int {
	t.Helper()
	var id int
	inWS(t, e, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `INSERT INTO workbook_rows (workbook_id, workspace_id, data) VALUES ($1,$2,'{}'::json) RETURNING id`, wb, ws).Scan(&id)
	})
	return id
}

func httpErr(t *testing.T, err error, status int, detail string) {
	t.Helper()
	var he *autosvc.HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("want HTTPError %d %q, got %v", status, detail, err)
	}
	if he.Status != status || he.Detail != detail {
		t.Fatalf("got %d %q, want %d %q", he.Status, he.Detail, status, detail)
	}
}

func TestCreateEnforcesMaxRules(t *testing.T) {
	e := setup(t)
	e.svc.Cfg.MaxRulesPerWS = 2
	for i := 0; i < 2; i++ {
		if _, err := e.svc.Create(e.ctx, wsA, ruleJSON(t, "on_row_added", "", "", "")); err != nil {
			t.Fatal(err)
		}
	}
	_, err := e.svc.Create(e.ctx, wsA, ruleJSON(t, "on_row_added", "", "", ""))
	httpErr(t, err, 422, "max rules per workspace reached (2)")
	// another workspace has its own budget
	if _, err := e.svc.Create(e.ctx, wsB, ruleJSON(t, "on_row_added", "", "", "")); err != nil {
		t.Fatal(err)
	}
}

func TestCreateScheduleBootstrapsMirrorAndJob(t *testing.T) {
	e := setup(t)
	in := ruleJSON(t, "on_schedule", `{"interval":"hourly"}`, `[]`, "")
	in.CreatedBy = "42"
	tr, err := e.svc.Create(e.ctx, wsA, in)
	if err != nil {
		t.Fatal(err)
	}
	if tr.CreatedBy == nil || *tr.CreatedBy != "42" {
		t.Fatalf("created_by %v", tr.CreatedBy)
	}
	if tr.ScheduleAnchor == nil || !tr.ScheduleAnchor.Equal(fixedNow) {
		t.Fatalf("schedule_anchor %v want %v", tr.ScheduleAnchor, fixedNow)
	}
	if tr.NextRunAt == nil {
		t.Fatal("next_run_at must be set after the post-commit bootstrap")
	}
	var fireKey, executor, payload string
	var cnt int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*), max(fire_key), max(executor), max(payload::text) FROM jobs WHERE workspace_id=$1 AND type='trigger_eval'`, wsA).Scan(&cnt, &fireKey, &executor, &payload); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 || !strings.HasPrefix(fireKey, "sched:"+tr.ID+":") || executor != "go" {
		t.Fatalf("job %d %q %q", cnt, fireKey, executor)
	}
	if !strings.Contains(payload, `"fire_source":"schedule"`) {
		t.Fatalf("payload %s", payload)
	}
	var mEnabled bool
	var mNext *time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT enabled, next_run_at FROM scheduled_triggers WHERE trigger_id=$1`, tr.ID).Scan(&mEnabled, &mNext); err != nil {
		t.Fatal(err)
	}
	if !mEnabled || mNext == nil || !mNext.Equal(*tr.NextRunAt) {
		t.Fatalf("mirror %v %v vs next_run_at %v", mEnabled, mNext, tr.NextRunAt)
	}

	// the MCP-shaped create stamps the same and returns the pre-bootstrap rule
	in2 := ruleJSON(t, "on_schedule", `{"interval":"daily"}`, `[]`, "")
	c, err := e.svc.CreateAutomation(e.ctx, wsA, in2)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(c)
	o, _ := wbcrud.ParseObject(b)
	id, _ := o.Get("automation_id")
	a, _ := o.Get("automation")
	if id != c.AutomationID() || id == "" {
		t.Fatalf("automation_id %v", id)
	}
	if v, _ := a.(*wbcrud.Obj).Get("next_run_at"); v != nil {
		t.Fatalf("MCP result predates the bootstrap, next_run_at = %v", v)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM scheduled_triggers WHERE trigger_id=$1`, c.AutomationID()).Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("mirror for MCP-created rule: %d %v", cnt, err)
	}
	// a disabled schedule rule bootstraps nothing
	in3 := ruleJSON(t, "on_schedule", `{"interval":"daily"}`, `[]`, "")
	in3.Enabled = false
	d, err := e.svc.Create(e.ctx, wsA, in3)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM scheduled_triggers WHERE trigger_id=$1`, d.ID).Scan(&cnt); err != nil || cnt != 0 {
		t.Fatalf("disabled rule must not be mirrored: %d %v", cnt, err)
	}
}

func TestPatchRevalidatesMergedRule(t *testing.T) {
	e := setup(t)
	seedWorkbook(t, e, wsA, "p13-wbA", `[{"id":"c1"},{"id":"c2"}]`)
	seedWorkbook(t, e, wsA, "p13-wbB", `[{"id":"c3"}]`)
	seedWorkbook(t, e, wsA, "p13-wbC", `null`)
	tr, err := e.svc.Create(e.ctx, wsA, ruleJSON(t, "on_row_added", "",
		`[{"type":"re_enrich","config":{"column_ids":["c1"]}}]`, `["p13-wbA"]`))
	if err != nil {
		t.Fatal(err)
	}
	// scope moves to a workbook without c1: the stored action no longer validates
	scope := []string{"p13-wbB"}
	_, err = e.svc.Patch(e.ctx, wsA, tr.ID, autosvc.PatchInput{ScopeWorkbookIDs: &scope})
	httpErr(t, err, 422, "re_enrich action 0: column_ids not in scoped workbook(s): ['c1']")
	// widening the scope validates again, with the action patched in the same call
	both := []string{"p13-wbA", "p13-wbB"}
	acts, _ := autosvc.ParseJSON([]byte(`[{"type":"re_enrich","config":{"column_ids":["c1","c3"]}}]`))
	al := acts.([]any)
	got, err := e.svc.Patch(e.ctx, wsA, tr.ID, autosvc.PatchInput{ScopeWorkbookIDs: &both, Actions: &al})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got.Actions)
	if !strings.Contains(string(b), `"c3"`) {
		t.Fatalf("actions %s", b)
	}
	// invalid condition: rejected and nothing applied
	bad := "{a} =="
	name := "renamed"
	_, err = e.svc.Patch(e.ctx, wsA, tr.ID, autosvc.PatchInput{Condition: &bad, Name: &name})
	httpErr(t, err, 422, "invalid condition: malformed comparison")
	cur, err := e.svc.Get(e.ctx, wsA, tr.ID)
	if err != nil || cur.Name != "r" || cur.Condition != "" {
		t.Fatalf("failed patch must change nothing: %+v %v", cur.Trigger, err)
	}
	// negative cap (D4)
	neg := -1.0
	_, err = e.svc.Patch(e.ctx, wsA, tr.ID, autosvc.PatchInput{MaxSpendUSDPerDay: &neg})
	httpErr(t, err, 422, "max_spend_usd_per_day must be a non-negative finite number")
	// trigger_config patch on a schedule rule re-checks the interval
	sch, err := e.svc.Create(e.ctx, wsA, ruleJSON(t, "on_schedule", `{"interval":"daily"}`, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	badCfg, _ := autosvc.ParseJSON([]byte(`{"interval":"monthly"}`))
	_, err = e.svc.Patch(e.ctx, wsA, sch.ID, autosvc.PatchInput{TriggerConfig: badCfg.(*wbcrud.Obj)})
	httpErr(t, err, 422, "invalid schedule interval 'monthly'")
}

func mirrorRow(t *testing.T, e *env, id string) (enabled bool, next *time.Time, found bool) {
	t.Helper()
	err := e.pool.QueryRow(e.ctx, `SELECT enabled, next_run_at FROM scheduled_triggers WHERE trigger_id=$1`, id).Scan(&enabled, &next)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return enabled, next, true
}

func TestPatchDisableUpdatesMirror(t *testing.T) {
	e := setup(t)
	tr, err := e.svc.Create(e.ctx, wsA, ruleJSON(t, "on_schedule", `{"interval":"hourly"}`, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	off := false
	got, err := e.svc.Patch(e.ctx, wsA, tr.ID, autosvc.PatchInput{Enabled: &off})
	if err != nil || got.Enabled {
		t.Fatalf("%v %+v", err, got)
	}
	if en, _, ok := mirrorRow(t, e, tr.ID); !ok || en {
		t.Fatalf("mirror after disable: enabled=%v found=%v", en, ok)
	}
	on := true
	got, err = e.svc.Patch(e.ctx, wsA, tr.ID, autosvc.PatchInput{Enabled: &on})
	if err != nil || !got.Enabled || got.NextRunAt == nil {
		t.Fatalf("%v %+v", err, got)
	}
	if en, next, _ := mirrorRow(t, e, tr.ID); !en || next == nil {
		t.Fatalf("mirror after enable: %v %v", en, next)
	}
	// a rename keeps the schedule untouched
	n := "renamed"
	if got, err = e.svc.Patch(e.ctx, wsA, tr.ID, autosvc.PatchInput{Name: &n}); err != nil || got.Name != "renamed" {
		t.Fatal(err)
	}
}

func TestDeleteRemovesMirror(t *testing.T) {
	e := setup(t)
	tr, err := e.svc.Create(e.ctx, wsA, ruleJSON(t, "on_schedule", `{"interval":"hourly"}`, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := mirrorRow(t, e, tr.ID); !ok {
		t.Fatal("mirror row missing after create")
	}
	if err := e.svc.Delete(e.ctx, wsA, tr.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := mirrorRow(t, e, tr.ID); ok {
		t.Fatal("mirror row must be deleted with the rule")
	}
	httpErr(t, e.svc.Delete(e.ctx, wsA, tr.ID), 404, "trigger not found")
}

func TestResumeReturnsNextRun(t *testing.T) {
	e := setup(t)
	tr, err := e.svc.Create(e.ctx, wsA, ruleJSON(t, "on_schedule", `{"interval":"weekly"}`, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Pause(e.ctx, wsA, tr.ID); err != nil {
		t.Fatal(err)
	}
	if en, _, _ := mirrorRow(t, e, tr.ID); en {
		t.Fatal("pause must disable the mirror")
	}
	next, err := e.svc.Resume(e.ctx, wsA, tr.ID)
	if err != nil || next == nil {
		t.Fatalf("resume: %v %v", next, err)
	}
	if next.Before(fixedNow) {
		t.Fatalf("next run %v must follow now %v", next, fixedNow)
	}
	if en, mn, _ := mirrorRow(t, e, tr.ID); !en || mn == nil || !mn.Equal(*next) {
		t.Fatalf("mirror %v %v vs %v", en, mn, next)
	}
	// a non-schedule rule resumes without a next run
	rc, err := e.svc.Create(e.ctx, wsA, ruleJSON(t, "on_row_added", "", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.Resume(e.ctx, wsA, rc.ID); err != nil || n != nil {
		t.Fatalf("%v %v", n, err)
	}
}

func TestManualRunPayloadEqualsPython(t *testing.T) {
	e := setup(t)
	seedWorkbook(t, e, wsA, "p13-wbA", `[]`)
	seedWorkbook(t, e, wsB, "p13-wbB", `[]`)
	r1 := seedRow(t, e, wsA, "p13-wbA")
	foreign := seedRow(t, e, wsB, "p13-wbB")
	tr, err := e.svc.Create(e.ctx, wsA, ruleJSON(t, "on_row_added", "", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	run := func(rows []string, dry bool) (int64, string, string, string) {
		var id int64
		var key string
		inWS(t, e, wsA, func(tx pgx.Tx) error {
			var err error
			id, key, err = e.svc.EnqueueManualRun(e.ctx, tx, wsA, tr.ID, rows, dry, fixedNow)
			return err
		})
		var payload, executor string
		if err := e.pool.QueryRow(e.ctx, `SELECT payload::text, executor FROM jobs WHERE id=$1`, id).Scan(&payload, &executor); err != nil {
			t.Fatal(err)
		}
		return id, key, payload, executor
	}
	itoa := func(n int) string { b, _ := json.Marshal(n); return string(b) }
	fk := "manual:" + tr.ID + ":2026-08-29T09:30:00.123456+00:00"
	head := `{"trigger_id":"` + tr.ID + `","workspace_id":"` + wsA + `","fire_source":"manual","fire_key":"` + fk + `","dry_run":`

	_, key, payload, executor := run(nil, false)
	if key != fk || payload != head+`false}` || executor != "go" {
		t.Fatalf("no targets: %s %s %s", key, payload, executor)
	}
	_, _, payload, _ = run([]string{itoa(r1), " " + itoa(r1) + " ", "9_9"}, true)
	want := head + `true,"targets":[{"workbook_id":"p13-wbA","row_id":"` + itoa(r1) + `"},{"workbook_id":"p13-wbA","row_id":"` + itoa(r1) + `"}]}`
	if payload != want {
		t.Fatalf("targets:\n got %s\nwant %s", payload, want)
	}
	// an id of another workspace matches nothing, but the key is still sent (empty list)
	_, _, payload, _ = run([]string{itoa(foreign)}, false)
	if payload != head+`false,"targets":[]}` {
		t.Fatalf("foreign row: %s", payload)
	}
	// unknown trigger
	var err2 error
	inWS(t, e, wsA, func(tx pgx.Tx) error {
		_, _, err2 = e.svc.EnqueueManualRun(e.ctx, tx, wsA, "nope", nil, false, fixedNow)
		return nil
	})
	httpErr(t, err2, 404, "trigger not found")
}

func TestNonNumericRowIDsAreNoMatchNotCrash(t *testing.T) {
	e := setup(t)
	seedWorkbook(t, e, wsA, "p13-wbA", `[]`)
	r1 := seedRow(t, e, wsA, "p13-wbA")
	_ = r1
	var got []autosvc.Target
	inWS(t, e, wsA, func(tx pgx.Tx) error {
		var err error
		got, err = e.svc.PreviewTargets(e.ctx, tx, wsA, []string{"abc", "", " ", "--1", "1.0", "1e3", "99999999999999999999", "-5", "0x10", "\x00", "5\n6"})
		return err
	})
	if len(got) != 0 {
		t.Fatalf("want no targets, got %+v", got)
	}
}

func TestTenantIsolationTriggersAndRuns(t *testing.T) {
	e := setup(t)
	tb, err := e.svc.Create(e.ctx, wsB, ruleJSON(t, "on_schedule", `{"interval":"daily"}`, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	inWS(t, e, wsB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(e.ctx, `INSERT INTO trigger_runs (id, workspace_id, trigger_id, status) VALUES ('run-b-1',$1,$2,'completed')`, wsB, tb.ID); err != nil {
			return err
		}
		_, err := tx.Exec(e.ctx, `INSERT INTO trigger_action_results (workspace_id, run_id, trigger_id, workbook_id, row_id, action_index, action_type, idempotency_key) VALUES ($1,'run-b-1',$2,'wb','1',0,'webhook','k1')`, wsB, tb.ID)
		return err
	})
	nf := "trigger not found"
	if _, err := e.svc.Get(e.ctx, wsA, tb.ID); err == nil {
		t.Fatal("get")
	} else {
		httpErr(t, err, 404, nf)
	}
	n := "x"
	_, err = e.svc.Patch(e.ctx, wsA, tb.ID, autosvc.PatchInput{Name: &n})
	httpErr(t, err, 404, nf)
	httpErr(t, e.svc.Delete(e.ctx, wsA, tb.ID), 404, nf)
	httpErr(t, e.svc.Pause(e.ctx, wsA, tb.ID), 404, nf)
	_, err = e.svc.Resume(e.ctx, wsA, tb.ID)
	httpErr(t, err, 404, nf)
	_, err = e.svc.Runs(e.ctx, wsA, tb.ID, 10)
	httpErr(t, err, 404, nf)
	var enqErr error
	inWS(t, e, wsA, func(tx pgx.Tx) error {
		_, _, enqErr = e.svc.EnqueueManualRun(e.ctx, tx, wsA, tb.ID, nil, false, fixedNow)
		return nil
	})
	httpErr(t, enqErr, 404, nf)
	_, err = e.svc.RunWithResults(e.ctx, wsA, "run-b-1")
	httpErr(t, err, 404, "run not found")
	// the owner still sees everything, and list is workspace scoped
	if rd, err := e.svc.RunWithResults(e.ctx, wsB, "run-b-1"); err != nil || len(rd.ActionResults) != 1 {
		t.Fatalf("%v %+v", err, rd)
	}
	if l, err := e.svc.List(e.ctx, wsA, autosvc.ListFilter{}); err != nil || len(l) != 0 {
		t.Fatalf("list A: %v %v", l, err)
	}
	d, err := e.svc.Get(e.ctx, wsB, tb.ID)
	if err != nil || len(d.RecentRuns) != 1 {
		t.Fatalf("get B: %v %+v", err, d)
	}
	if _, err := e.svc.Runs(e.ctx, wsB, tb.ID, -1); err == nil {
		t.Fatal("negative limit must be a 422")
	}
	// B's rule is untouched by A's attempts and its mirror survives
	if _, _, ok := mirrorRow(t, e, tb.ID); !ok {
		t.Fatal("mirror of the foreign rule disappeared")
	}
}

// Final review M5: the header_secret_ref lookup must reuse the validation
// transaction. With a one-connection pool, a second InWorkspace acquire inside
// the open transaction can never be served and the request hangs.
func TestValidateSecretLookupReusesTransaction(t *testing.T) {
	pool := testpg.NewAppPool(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, slug) VALUES ($1::varchar,$1::text,$1::text) ON CONFLICT DO NOTHING`, wsA); err != nil {
		t.Fatal(err)
	}
	if err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workspace_settings (workspace_id, key, value) VALUES ($1,'P08_M5_KEY','v') ON CONFLICT (workspace_id, key) DO UPDATE SET value='v'`, wsA)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tenancy.InWorkspace(context.Background(), pool, wsA, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `DELETE FROM workspace_settings WHERE key='P08_M5_KEY'`)
			return err
		})
	})
	svc := &autosvc.Service{Pool: pool, Now: func() time.Time { return fixedNow },
		Cfg: autosvc.Config{MaxRulesPerWS: 50, MaxActionsPerRule: 10,
			Secrets: secrets.NewResolver(nil, secrets.NewPgStore(pool), os.Getenv)}}
	tt := "on_row_added"
	in := autosvc.RuleInput{Name: "r", TriggerType: &tt, Enabled: true}
	a, err := autosvc.ParseJSON([]byte(`[{"type":"webhook","config":{"url":"https://hooks.example.com/x","header_secret_ref":"P08_M5_KEY"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	in.Actions = a.([]any)
	err = tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error { return svc.Validate(ctx, tx, wsA, in) })
	if err != nil {
		t.Fatalf("validate with a known secret ref on a 1-connection pool: %v", err)
	}
}
