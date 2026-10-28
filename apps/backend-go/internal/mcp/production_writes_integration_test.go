//go:build integration

package mcp

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachsend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// fakeSecretChecker is the workspace secret lookup of automation validation
// (secrets.Resolver.Has in production).
type fakeSecretChecker map[string]bool

func (f fakeSecretChecker) Has(_ context.Context, _ string, key string) (bool, error) { return f[key], nil }

// pwEnv is a workspace served by the PRODUCTION outreach and automation
// adapters (NewProductionDeps) through BuildRegistry and the write pipeline.
type pwEnv struct {
	*wEnv
	srv   *Server
	seq   string
	other string // a second workspace with its own sequence
	oseq  string
	leads map[string]int64 // company -> id
}

func newPWEnv(t *testing.T, mut func(*Config, *WriteAdapters)) *pwEnv {
	t.Helper()
	w := newWEnv(t, "admin")
	e := &pwEnv{wEnv: w, leads: map[string]int64{}}
	ctx := context.Background()
	e.other = w.workspace(t, "other", &w.uid, "")
	e.seq, e.oseq = "seq-"+w.sfx, "seq-o-"+w.sfx
	exec := func(ws, q string, args ...any) {
		t.Helper()
		if err := w.d.RunInWorkspace(ctx, ws, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q, args...); return err }); err != nil {
			t.Fatal(err)
		}
	}
	addSeq := func(ws, id string) {
		exec(ws, `INSERT INTO outreach_sequences (id, workspace_id, name, description, steps, status, created_at, updated_at)
			VALUES ($1,$2,'MCP sequence','','[{"subject": "Hi", "body": "x"}]'::json,'active',now(),now())`, id, ws)
	}
	addSeq(w.ws, e.seq)
	addSeq(e.other, e.oseq)
	exec(w.ws, `INSERT INTO outreach_suppressions (workspace_id, email, reason, source, locked, created_at)
		VALUES ($1,'stop@sup.example.com','unsubscribe','seed',false,now())`, w.ws)
	for _, l := range []struct{ ws, company, email string }{
		{w.ws, "Acme", "Info@Acme.example.com"}, {w.ws, "Globex", "hello@globex.example.com"},
		{w.ws, "NoMail", ""}, {w.ws, "Suppressed", "stop@sup.example.com"}, {e.other, "Foreign", "x@foreign.example.com"},
	} {
		var id int64
		if err := w.d.RunInWorkspace(ctx, l.ws, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `INSERT INTO leads (workspace_id, company, city, email, contact_person, created_at, updated_at)
				VALUES ($1,$2,'Berlin',$3,'Ada','2026-08-29T10:00:00','2026-08-29T10:00:00') RETURNING id`, l.ws, l.company, l.email).Scan(&id)
		}); err != nil {
			t.Fatal(err)
		}
		e.leads[l.company] = id
	}
	t.Cleanup(func() {
		for _, ws := range []string{w.ws, e.other} {
			_ = w.d.RunInWorkspace(ctx, ws, func(tx pgx.Tx) error {
				for _, q := range []string{`DELETE FROM jobs`, `DELETE FROM outreach_enrollments`, `DELETE FROM outreach_suppressions`,
					`DELETE FROM outreach_sequences`, `DELETE FROM triggers`, `DELETE FROM leads`, `DELETE FROM mcp_audit_log`,
					`DELETE FROM trigger_cap_reservations`} {
					_, _ = tx.Exec(ctx, q+` WHERE workspace_id=$1`, ws)
				}
				return nil
			})
		}
	})
	w.cfg = Config{WriteEnabled: true, AutomationsEnabled: true, AutomationsAllowLegacyOutreach: true, AutomationsMaxRulesPerWS: 50}
	wa := WriteAdapters{
		Secrets:  fakeSecretChecker{"WEBHOOK_TOKEN": true, "SMTP_PASSWORD": true},
		Outbound: secrets.NewOutboundPolicy(nil, func(string) bool { return false }),
	}
	if mut != nil {
		mut(&w.cfg, &wa)
	}
	verify := emailverify.NewDeps()
	verify.SMTPDisabled = true
	d := NewProductionDeps(w.cfg, w.pool, nil, verify, wa)
	w.d = d
	e.srv = NewServer(d, BuildRegistry(d))
	return e
}

func (e *pwEnv) call(name string, args Args) string { return e.wEnv.call(e.srv, name, args) }

func (e *pwEnv) one(t *testing.T, ws, q string, dst ...any) {
	t.Helper()
	if err := e.d.RunInWorkspace(context.Background(), ws, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), q, ws).Scan(dst...)
	}); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (e *pwEnv) n(t *testing.T, q string) int {
	t.Helper()
	var n int
	e.one(t, e.ws, q, &n)
	return n
}

func TestProdOutreachToolsAreAvailable(t *testing.T) {
	e := newPWEnv(t, nil)
	if e.d.Outreach == nil || e.d.Automation == nil {
		t.Fatal("production deps must wire the outreach and automation adapters")
	}
	for _, c := range []struct {
		tool string
		args Args
	}{
		{"enroll_leads", Args{"sequence_id": e.seq, "lead_ids": []any{}}},
		{"send_email", Args{"lead_id": float64(e.leads["NoMail"])}},
		{"create_automation", Args{"name": "x", "trigger_type": "on_signal"}},
	} {
		if got := e.call(c.tool, c.args); strings.Contains(got, "not available") {
			t.Errorf("%s: %s", c.tool, got)
		}
	}
}

func TestProdEnrollLeads(t *testing.T) {
	e := newPWEnv(t, nil)
	ids := []any{float64(e.leads["Acme"]), float64(e.leads["NoMail"]), float64(e.leads["Suppressed"]),
		float64(e.leads["Foreign"]), float64(99999999999), float64(-1)}
	got := e.call("enroll_leads", Args{"sequence_id": e.seq, "lead_ids": ids})
	want := `{"enrolled": 1, "skipped": [` +
		`{"lead_id": ` + strconv.FormatInt(e.leads["NoMail"], 10) + `, "reason": "no_email"}, ` +
		`{"lead_id": ` + strconv.FormatInt(e.leads["Suppressed"], 10) + `, "reason": "suppressed"}, ` +
		`{"lead_id": ` + strconv.FormatInt(e.leads["Foreign"], 10) + `, "reason": "no_email"}, ` +
		`{"lead_id": 99999999999, "reason": "no_email"}, {"lead_id": -1, "reason": "no_email"}], "total_lead_ids": 6}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	var src, snap string
	var consent bool
	e.one(t, e.ws, `SELECT consent_source, to_email_snapshot, consent_at IS NOT NULL FROM outreach_enrollments WHERE workspace_id=$1`, &src, &snap, &consent)
	if src != "mcp_enroll" || snap != "info@acme.example.com" || !consent {
		t.Fatalf("enrollment %q %q consent_at=%v", src, snap, consent)
	}
	again := e.call("enroll_leads", Args{"sequence_id": e.seq, "lead_ids": []any{float64(e.leads["Acme"])}})
	if !strings.Contains(again, `"reason": "already_enrolled"`) || !strings.HasPrefix(again, `{"enrolled": 0`) {
		t.Fatal(again)
	}
	// Another workspace's sequence is a miss: nothing is written and the cap slot is freed.
	if got := e.call("enroll_leads", Args{"sequence_id": e.oseq, "lead_ids": []any{float64(e.leads["Globex"])}}); got != `{"error": "Sequence not found"}` {
		t.Fatal(got)
	}
	if n := e.n(t, `SELECT count(*) FROM outreach_enrollments WHERE workspace_id=$1`); n != 1 {
		t.Fatalf("enrollments %d", n)
	}
	if e.held(t) != 0 {
		t.Fatal("held reservation left behind")
	}
}

type sendJobRow struct {
	Executor, FireKey string
	Payload           map[string]any
}

func (e *pwEnv) sendJobs(t *testing.T) []sendJobRow {
	t.Helper()
	var out []sendJobRow
	err := e.d.RunInWorkspace(context.Background(), e.ws, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT executor, coalesce(fire_key,''), payload::text FROM jobs
			WHERE workspace_id=$1 AND type='send' ORDER BY id`, e.ws)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r sendJobRow
			var p string
			if err := rows.Scan(&r.Executor, &r.FireKey, &p); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(p), &r.Payload); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestProdSendEmailEnqueuesOneGoSendJob(t *testing.T) {
	e := newPWEnv(t, nil)
	lead := float64(e.leads["Acme"])
	args := Args{"lead_id": lead, "subject": "Hello", "body_html": "<p>hi</p>"}
	if got := e.call("send_email", args); got != `{"status": "success", "summary": "send enqueued"}` {
		t.Fatal(got)
	}
	jobs := e.sendJobs(t)
	if len(jobs) != 1 {
		t.Fatalf("jobs %d", len(jobs))
	}
	j := jobs[0]
	key := "trig:" + e.ws + ":adhoc:" + strconv.FormatInt(e.leads["Acme"], 10) + ":0"
	if j.Executor != "go" || j.FireKey != key || j.Payload["idempotency_key"] != key {
		t.Fatalf("job %+v", j)
	}
	p := j.Payload
	if p["workspace_id"] != e.ws || p["to_email"] != "info@acme.example.com" || p["subject"] != "Hello" ||
		p["body_html"] != "<p>hi</p>" || p["sequence_id"] != nil || p["enrollment_id"] != nil ||
		p["lead_id"] != lead || p["step_number"] != float64(0) {
		t.Fatalf("payload %v", p)
	}
	if v, _ := p["variables"].(map[string]any); v["company"] != "Acme" || v["contact_person"] != "Ada" {
		t.Fatalf("variables %v", p["variables"])
	}
	// At most once: the same ad-hoc send is not queued again.
	if got := e.call("send_email", args); got != `{"status": "success", "summary": "already queued/sent"}` {
		t.Fatal(got)
	}
	if len(e.sendJobs(t)) != 1 {
		t.Fatal("second job queued")
	}
}

func TestProdSendEmailSequenceStepUsesStepKey(t *testing.T) {
	e := newPWEnv(t, nil)
	got := e.call("send_email", Args{"lead_id": float64(e.leads["Globex"]), "sequence_id": e.seq, "step_number": float64(1),
		"subject": "Step", "body_html": "b", "idempotency_key": "k-1"})
	if got != `{"status": "success", "summary": "send enqueued"}` {
		t.Fatal(got)
	}
	var eid int
	var src string
	e.one(t, e.ws, `SELECT id, consent_source FROM outreach_enrollments WHERE workspace_id=$1`, &eid, &src)
	jobs := e.sendJobs(t)
	if len(jobs) != 1 || src != "trigger_send" {
		t.Fatalf("jobs %d source %q", len(jobs), src)
	}
	want := outreachsend.SequenceStepIdem(e.ws, e.seq, eid, 1)
	if jobs[0].FireKey != want || jobs[0].Payload["enrollment_id"] != float64(eid) || jobs[0].Payload["sequence_id"] != e.seq {
		t.Fatalf("job %+v want key %s", jobs[0], want)
	}
}

func TestProdSendEmailRefusalsWriteNothing(t *testing.T) {
	e := newPWEnv(t, nil)
	cases := []struct {
		name string
		args Args
		want string
	}{
		{"suppressed", Args{"lead_id": float64(e.leads["Suppressed"])},
			`{"status": "skipped", "summary": "suppressed", "skip_reason": "suppressed"}`},
		{"foreign lead", Args{"lead_id": float64(e.leads["Foreign"])},
			`{"status": "skipped", "summary": "lead has no email", "skip_reason": "no_email"}`},
		{"foreign sequence", Args{"lead_id": float64(e.leads["Acme"]), "sequence_id": e.oseq},
			`{"status": "failed", "summary": "", "skip_reason": "not_connected", "error": "sequence not found in workspace"}`},
		{"lead id out of range", Args{"lead_id": float64(99999999999)},
			`{"status": "skipped", "summary": "lead has no email", "skip_reason": "no_email"}`},
	}
	for _, c := range cases {
		if got := e.call("send_email", c.args); got != c.want {
			t.Errorf("%s: %s", c.name, got)
		}
	}
	if len(e.sendJobs(t)) != 0 || e.n(t, `SELECT count(*) FROM outreach_enrollments WHERE workspace_id=$1`) != 0 {
		t.Fatal("a refused send wrote a job or an enrollment")
	}
	if e.held(t) != 0 {
		t.Fatal("held reservation")
	}
}

func TestProdSendEmailExistingEnrollmentWithOtherEmailFails(t *testing.T) {
	e := newPWEnv(t, nil)
	if err := e.d.RunInWorkspace(context.Background(), e.ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO outreach_enrollments (workspace_id, sequence_id, lead_id, to_email_snapshot, status, current_step, sent_count, soft_bounce_count, error)
			VALUES ($1,$2,$3,'old@acme.example.com','pending',0,0,0,'')`, e.ws, e.seq, e.leads["Acme"])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got := e.call("send_email", Args{"lead_id": float64(e.leads["Acme"]), "sequence_id": e.seq})
	if !strings.Contains(got, `"status": "failed"`) || !strings.Contains(got, "different email") {
		t.Fatal(got)
	}
	if len(e.sendJobs(t)) != 0 {
		t.Fatal("job queued to a stale enrollment")
	}
}

func TestProdSendEmailGateAndCapability(t *testing.T) {
	e := newPWEnv(t, func(c *Config, _ *WriteAdapters) { c.AutomationsAllowLegacyOutreach = false })
	if got := e.call("send_email", Args{"lead_id": float64(e.leads["Acme"])}); got != `{"error": "legacy_outreach_disabled"}` {
		t.Fatal(got)
	}
	e.ctx.Capabilities = capSet(CapLeadsRead, CapSequencesEnroll, CapAutomationsWrite)
	if got := e.call("send_email", Args{"lead_id": float64(e.leads["Acme"])}); !strings.Contains(got, "capability not granted") {
		t.Fatal(got)
	}
	e.ctx.Capabilities = capSet(CapLeadsRead, CapOutreachSend)
	for _, tool := range []string{"enroll_leads", "create_automation"} {
		if got := e.call(tool, Args{"sequence_id": e.seq, "name": "x"}); !strings.Contains(got, "capability not granted") {
			t.Errorf("%s: %s", tool, got)
		}
	}
	if len(e.sendJobs(t)) != 0 {
		t.Fatal("job queued")
	}
}

func TestProdCreateAutomationStoresOrderedRuleAndBootstraps(t *testing.T) {
	e := newPWEnv(t, nil)
	body := `{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "create_automation", "arguments": ` +
		`{"name": "Daily sweep", "trigger_type": "on_schedule", "trigger_config": {"interval": "daily", "at": "09:00"}, ` +
		`"actions": [{"type": "webhook", "config": {"url": "https://hooks.example.com/x", "method": "POST"}}], "max_actions_per_day": 20}}}`
	resp, _ := e.srv.Handle(context.Background(), e.ctx, []byte(body))
	var env struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &env); err != nil || len(env.Result.Content) != 1 {
		t.Fatalf("%s: %v", resp, err)
	}
	text := env.Result.Content[0].Text
	if !strings.HasPrefix(text, `{"status": "created", "automation_id": "`) ||
		!strings.Contains(text, `"actions": [{"type": "webhook", "config": {"url": "https://hooks.example.com/x", "method": "POST"}}]`) ||
		!strings.Contains(text, `"trigger_config": {"interval": "daily", "at": "09:00"}`) ||
		!strings.Contains(text, `"created_by": "`+strconv.Itoa(int(e.uid))+`"`) || !strings.Contains(text, `"workspace_id": "`+e.ws+`"`) {
		t.Fatal(text)
	}
	var actions, createdBy string
	var anchored, scheduled bool
	e.one(t, e.ws, `SELECT actions::text, created_by, schedule_anchor IS NOT NULL, next_run_at IS NOT NULL FROM triggers WHERE workspace_id=$1`,
		&actions, &createdBy, &anchored, &scheduled)
	if !strings.HasPrefix(actions, `[{"type"`) || createdBy != strconv.Itoa(int(e.uid)) || !anchored {
		t.Fatalf("stored actions %s created_by %s anchored %v", actions, createdBy, anchored)
	}
	if !scheduled {
		t.Fatal("enabled on_schedule rule was not bootstrapped after the commit")
	}
}

func TestProdCreateAutomationValidation(t *testing.T) {
	e := newPWEnv(t, nil)
	hook := func(cfg map[string]any) Args {
		return Args{"name": "x", "trigger_type": "on_signal", "actions": []any{map[string]any{"type": "webhook", "config": cfg}}}
	}
	cases := []struct {
		name string
		args Args
		want string
	}{
		{"registered credential ref", hook(map[string]any{"url": "https://h.example.com/x", "header_secret_ref": "SMTP_PASSWORD"}),
			`{"error": "header_secret_ref is not an available webhook secret", "status_code": 422}`},
		{"unknown ref", hook(map[string]any{"url": "https://h.example.com/x", "header_secret_ref": "WEBHOOK_NOPE"}),
			`{"error": "header_secret_ref is not an available webhook secret", "status_code": 422}`},
		{"legacy outreach action", Args{"name": "x", "trigger_type": "on_signal", "actions": []any{map[string]any{"type": "send_email", "config": map[string]any{}}}},
			`{"error": "legacy_outreach_unavailable", "status_code": 409}`},
		{"name too long", Args{"name": strings.Repeat("n", 201), "trigger_type": "on_signal"},
			`{"error": "name too long (max 200 characters)", "status_code": 422}`},
		{"too many actions", Args{"name": "x", "trigger_type": "on_signal", "actions": []any{
			map[string]any{"type": "webhook", "config": map[string]any{"url": "https://a.example.com"}},
			map[string]any{"type": "webhook", "config": map[string]any{"url": "https://b.example.com"}}}},
			`{"error": "too many actions (max 1)", "status_code": 422}`},
		{"max_actions_per_day out of range", Args{"name": "x", "trigger_type": "on_signal", "max_actions_per_day": float64(1 << 40)},
			`{"error": "max_actions_per_day must be at most 2147483647", "status_code": 422}`},
	}
	e2 := newPWEnv(t, func(_ *Config, w *WriteAdapters) { w.MaxActionsPerRule = 1 })
	for _, c := range cases {
		env := e
		if c.name == "too many actions" {
			env = e2
		}
		if got := env.call("create_automation", c.args); got != c.want {
			t.Errorf("%s: %s", c.name, got)
		}
	}
	if e.n(t, `SELECT count(*) FROM triggers WHERE workspace_id=$1`) != 0 || e.held(t) != 0 {
		t.Fatal("a rejected rule was stored or held a slot")
	}
	ok := e.call("create_automation", hook(map[string]any{"url": "https://h.example.com/x", "header_secret_ref": "WEBHOOK_TOKEN"}))
	if !strings.HasPrefix(ok, `{"status": "created"`) {
		t.Fatal(ok)
	}
}

func TestProdCreateAutomationRuleCap(t *testing.T) {
	e := newPWEnv(t, func(c *Config, _ *WriteAdapters) { c.AutomationsMaxRulesPerWS = 1 })
	if got := e.call("create_automation", Args{"name": "a", "trigger_type": "on_signal"}); !strings.HasPrefix(got, `{"status": "created"`) {
		t.Fatal(got)
	}
	if got := e.call("create_automation", Args{"name": "b", "trigger_type": "on_signal"}); got != `{"error": "max rules per workspace reached (1)"}` {
		t.Fatal(got)
	}
	var other int
	e.one(t, e.other, `SELECT count(*) FROM triggers WHERE workspace_id=$1`, &other)
	if other != 0 || e.n(t, `SELECT count(*) FROM triggers WHERE workspace_id=$1`) != 1 {
		t.Fatal("rule stored outside the token workspace or past the cap")
	}
}
