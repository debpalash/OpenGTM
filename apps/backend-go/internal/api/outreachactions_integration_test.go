//go:build integration

package api

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// oactEnv is the contract seed of scripts/contract_seed.py (_seed_outreach) behind the sequence CRUD and action routes.
type oactEnv struct {
	oseqEnv
	res *secrets.Resolver
	t   *testing.T
}

type oactEnrollment struct {
	ws, seq string
	lead    int
	email   string
	source  string
	step    int
	status  string
	next    any
	sent    int
	soft    int
	last    any
	err     string
}

var oactEnrollments = []oactEnrollment{
	{"ws-alpha", "seq-out-a-01", 4, "info@eco.example.com", "manual_enroll", 0, "pending", "2026-08-29 08:00:00+00", 0, 0, nil, ""},
	{"ws-alpha", "seq-out-a-01", 5, "kontakt@nordlicht.example.com", "manual_enroll", 1, "scheduled", "2026-08-29 09:00:00+00", 1, 0, "2026-08-29 08:10:00+00", ""},
	{"ws-alpha", "seq-out-a-01", 6, "hello@zenithrobotics.example.com", "import", 1, "sent", nil, 1, 0, "2026-08-29 08:20:00+00", ""},
	{"ws-alpha", "seq-out-a-01", 8, "sales@zenithrobotics.example.com", "import", 1, "opened", nil, 1, 0, "2026-08-29 08:30:00+00", ""},
	{"ws-alpha", "seq-out-a-01", 10, "info@quantumfoods.example.com", "import", 1, "replied", nil, 1, 0, "2026-08-29 08:40:00+00", ""},
	{"ws-alpha", "seq-out-a-01", 14, "ceo@hotpursuit.example.com", "manual_enroll", 0, "bounced", nil, 1, 3, "2026-08-29 08:50:00+00", "soft_bounce_max"},
	{"ws-alpha", "seq-out-a-01", 15, "hi@berlinbakery.example.com", "manual_enroll", 0, "failed", nil, 0, 0, nil, "connection refused"},
	{"ws-alpha", "seq-out-a-01", 16, "+cmd@sheet.example.com", "manual_enroll", 0, "suppressed", nil, 0, 0, nil, ""},
	{"ws-alpha", "seq-out-a-01", 1, "info@acme01.example.com", "manual_enroll", 2, "completed", nil, 3, 0, "2026-08-29 09:30:00+00", ""},
	{"ws-alpha", "seq-out-a-01", 2, "hello@globex.example.com", "manual_enroll", 0, "skipped", nil, 0, 0, nil, ""},
	{"ws-beta", "seq-out-b-01", 17, "beta@zenithrobotics.example.com", "manual_enroll", 0, "scheduled", "2026-08-29 09:00:00+00", 1, 0, "2026-08-29 08:45:00+00", ""},
	{"ws-beta", "seq-out-b-01", 18, "soft-target@contract.example.com", "manual_enroll", 0, "scheduled", "2026-08-29 09:00:00+00", 3, 0, "2026-08-29 08:46:00+00", ""},
	{"ws-beta", "seq-out-b-01", 3, "complaint-target@contract.example.com", "manual_enroll", 0, "scheduled", "2026-08-29 09:00:00+00", 1, 0, "2026-08-29 08:47:00+00", ""},
}

type oactSend struct {
	ws, seq                string
	enr, lead, step        int
	to, subject, status    string
	skip, msgID, idem, err string
	sent, opened, replied  any
	bounced                any
	created                string
}

var oactSends = []oactSend{
	{"ws-alpha", "seq-out-a-01", 2, 5, 0, "kontakt@nordlicht.example.com", "Hi Nordlicht", "sent", "", "<seed-0001@contract.example.com>", "seq:ws-alpha:seq-out-a-01:2:0", "", "2026-08-29 08:10:00+00", nil, nil, nil, "2026-08-29 08:10:00+00"},
	{"ws-alpha", "seq-out-a-01", 3, 6, 0, "hello@zenithrobotics.example.com", "Hi Zenith", "in_flight", "", "", "seq:ws-alpha:seq-out-a-01:3:0", "", nil, nil, nil, nil, "2026-08-29 08:20:00+00"},
	{"ws-alpha", "seq-out-a-01", 7, 15, 0, "hi@berlinbakery.example.com", "Hi Bakery", "failed", "", "", "seq:ws-alpha:seq-out-a-01:7:0", "connection refused", nil, nil, nil, nil, "2026-08-29 08:25:00+00"},
	{"ws-alpha", "seq-out-a-01", 6, 14, 0, "ceo@hotpursuit.example.com", "Hi Hot", "bounced", "", "<seed-0004@contract.example.com>", "seq:ws-alpha:seq-out-a-01:6:0", "550 5.1.1 user unknown", "2026-08-29 08:50:00+00", nil, nil, "2026-08-29 08:55:00+00", "2026-08-29 08:50:00+00"},
	{"ws-alpha", "seq-out-a-01", 8, 16, 0, "+cmd@sheet.example.com", "Hi Sheet", "skipped", "suppressed", "", "seq:ws-alpha:seq-out-a-01:8:0", "", nil, nil, nil, nil, "2026-08-29 08:56:00+00"},
	{"ws-alpha", "seq-out-a-01", 10, 2, 0, "hello@globex.example.com", "Hi Globex", "skipped", "replay", "", "seq:ws-alpha:seq-out-a-01:10:0", "", nil, nil, nil, nil, "2026-08-29 08:57:00+00"},
	{"ws-alpha", "seq-out-a-01", 9, 1, 0, "info@acme01.example.com", "Hi Acme", "sent", "", "<seed-0007@contract.example.com>", "seq:ws-alpha:seq-out-a-01:9:0", "", "2026-08-29 08:30:00+00", nil, nil, nil, "2026-08-29 08:30:00+00"},
	{"ws-alpha", "seq-out-a-01", 9, 1, 1, "info@acme01.example.com", "Following up", "sent", "", "<seed-0008@contract.example.com>", "seq:ws-alpha:seq-out-a-01:9:1", "", "2026-08-29 09:00:00+00", nil, nil, nil, "2026-08-29 09:00:00+00"},
	{"ws-alpha", "seq-out-a-01", 9, 1, 2, "info@acme01.example.com", "Last note", "sent", "", "<seed-0009@contract.example.com>", "seq:ws-alpha:seq-out-a-01:9:2", "", "2026-08-29 09:30:00+00", nil, nil, nil, "2026-08-29 09:30:00+00"},
	{"ws-alpha", "seq-out-a-01", 4, 8, 0, "sales@zenithrobotics.example.com", "Hi Zenith Sales", "sent", "", "<seed-0010@contract.example.com>", "seq:ws-alpha:seq-out-a-01:4:0", "", "2026-08-29 08:35:00+00", "2026-08-29 08:40:00+00", nil, nil, "2026-08-29 08:35:00+00"},
	{"ws-alpha", "seq-out-a-01", 5, 10, 0, "info@quantumfoods.example.com", "Hi Quantum", "sent", "", "<seed-0011@contract.example.com>", "seq:ws-alpha:seq-out-a-01:5:0", "", "2026-08-29 08:40:00+00", "2026-08-29 08:41:00+00", "2026-08-29 08:45:00+00", nil, "2026-08-29 08:40:00+00"},
}

var oactSuppress = [][]any{
	{"ws-alpha", "locked-unsub@example.com", "unsubscribe", "seq-out-a-01", true, "2026-08-29 08:01:00+00"},
	{"ws-alpha", "locked-complaint@example.com", "complaint", "inbound", true, "2026-08-29 08:02:00+00"},
	{"ws-alpha", "manual-removable@example.com", "manual", "manual", false, "2026-08-29 08:03:00+00"},
	{"ws-alpha", "bounced-user@example.com", "bounce", "seq-out-a-01", false, "2026-08-29 08:04:00+00"},
	{"ws-alpha", "patlee@gmail.com", "manual", "manual", false, "2026-08-29 08:05:00+00"},
	{"ws-alpha", "+cmd@sheet.example.com", "manual", "manual", false, "2026-08-29 08:06:00+00"},
	{"ws-beta", "beta-suppressed@example.com", "manual", "manual", false, "2026-08-29 08:07:00+00"},
}

func oactPurge() {
	oseqPurge()
	for _, q := range []string{`DELETE FROM outreach_suppressions WHERE workspace_id IN ('ws-alpha','ws-beta')`,
		// the explicit-id leads seeded by newOactEnv (19, 20) must not outlive the test: another package's default-id insert would collide
		`DELETE FROM leads WHERE workspace_id = 'ws-alpha' AND id IN (19, 20)`,
		`DELETE FROM workspace_settings WHERE workspace_id IN ('ws-alpha','ws-beta')`,
		`DELETE FROM jobs WHERE type = 'send' AND workspace_id IN ('ws-alpha','ws-beta')`,
		`DROP TRIGGER IF EXISTS oact_fail_schedule ON outreach_schedules`} {
		wcrudOwnerExecQuiet(q)
	}
}

// newOactEnv seeds the recorded rows (ids as in the recording: enrollments 1-13, sends 1-11 for ws-alpha) and serves the
// sequence CRUD and action routes.
func newOactEnv(t *testing.T, getenv map[string]string, outreach config.OutreachSettings) oactEnv {
	t.Helper()
	e := newLeadsEnv(t)
	e.seed(t, "erin", pythonHash, true, "user", false)
	e.member(t, "ws-alpha", e.mustUserByName(t, "erin").ID, "editor")
	oactPurge()
	t.Cleanup(oactPurge)
	for i, c := range [][]string{{"Gmail Dot Plus Co", "Pat.Lee+Sales@Gmail.com", "Pat Lee"}, {"Mixed Case Co", "Mixed.Case@Example.COM", "Mia Case"}} {
		ownerExec(t, `INSERT INTO leads (id, workspace_id, company, city, website, email, phone, contact_person, source, score, score_tier, status, created_at, updated_at)
			VALUES ($1,'ws-alpha',$2,'Austin','',$3,'',$4,'contract',10,'cold','new','2026-08-24T09:00:00','2026-08-24T09:00:00')`, 19+i, c[0], c[1], c[2])
	}
	ownerExec(t, `INSERT INTO outreach_sequences (id, workspace_id, name, description, steps, status, daily_limit, send_window_start,
		send_window_end, send_window_tz, consent_basis, bounce_count, complaint_count, auto_paused, created_at, updated_at) VALUES
		('seq-out-a-01','ws-alpha','Three step cold outreach','primary contract sequence',CAST($1 AS json),'draft',50,9,18,'UTC','legitimate_interest',2,0,false,'2026-08-29 08:00:00+00','2026-08-29 08:00:00+00'),
		('seq-out-a-02','ws-alpha','Draft without consent','',CAST('[]' AS json),'draft',25,8,17,'Europe/Berlin','',0,0,false,'2026-08-29 07:00:00+00','2026-08-29 07:00:00+00'),
		('seq-out-b-01','ws-beta','Beta sequence','beta tenant',CAST('[{"step_number": 0, "subject": "Beta intro", "body_html": "<p>Hello from beta.</p>", "delay_hours": 0}]' AS json),'draft',50,9,18,'UTC','consent_contract',0,0,false,'2026-08-29 08:30:00+00','2026-08-29 08:30:00+00')`, oseqStepsA1)
	for i, en := range oactEnrollments {
		ownerExec(t, `INSERT INTO outreach_enrollments (id, workspace_id, sequence_id, lead_id, to_email_snapshot, consent_source, consent_at, current_step, status,
			next_send_at, sent_count, soft_bounce_count, last_sent_at, error) VALUES ($1,$2,$3,$4,$5,$6,'2026-08-29 07:30:00+00',$7,$8,$9::timestamptz,$10,$11,$12::timestamptz,$13)`,
			i+1, en.ws, en.seq, en.lead, en.email, en.source, en.step, en.status, en.next, en.sent, en.soft, en.last, en.err)
	}
	for i, s := range oactSends {
		ownerExec(t, `INSERT INTO outreach_sends (id, workspace_id, sequence_id, enrollment_id, lead_id, step_number, to_email, subject, status, skip_reason,
			message_id, idempotency_key, charged_usd, migrated, error, sent_at, opened_at, replied_at, bounced_at, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),$11,$12,0,false,$13,$14::timestamptz,$15::timestamptz,$16::timestamptz,$17::timestamptz,$18::timestamptz,$18::timestamptz)`,
			i+1, s.ws, s.seq, s.enr, s.lead, s.step, s.to, s.subject, s.status, s.skip, s.msgID, s.idem, s.err, s.sent, s.opened, s.replied, s.bounced, s.created)
	}
	for _, r := range oactSuppress {
		ownerExec(t, `INSERT INTO outreach_suppressions (workspace_id, email, reason, source, locked, created_at) VALUES ($1,$2,$3,$4,$5,$6::timestamptz)`, r...)
	}
	ownerExec(t, `SELECT setval(pg_get_serial_sequence('outreach_enrollments','id'), GREATEST(1000, (SELECT max(id) FROM outreach_enrollments)))`)
	ownerExec(t, `SELECT setval(pg_get_serial_sequence('outreach_sends','id'), GREATEST(1000, (SELECT max(id) FROM outreach_sends)))`)

	env, err := secrets.NewEnvelope(secrets.Config{MasterKey: "oact-test-key", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	get := func(k string) string { return getenv[k] }
	outreach.GoRoutesEnabled = true
	if outreach.TickInterval == 0 {
		outreach.TickInterval = 900
	}
	if outreach.TickMaxEnqueue == 0 {
		outreach.TickMaxEnqueue = 200
	}
	d := Deps{Pool: e.pool, Signer: testSigner, Identity: e.st, Envelope: env, Getenv: get,
		Flags: config.Flags{PGLeadStore: true, Outreach: outreach}}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(e.pool, nil))
	registerOutreachSequences(r, d)
	registerOutreachActions(r, d)
	e.h = r.Handler()
	return oactEnv{oseqEnv: oseqEnv{e}, res: secrets.NewResolver(env, secrets.NewPgStore(e.pool), get), t: t}
}

func (e oactEnv) setSecret(ws, k, v string) {
	e.t.Helper()
	if err := e.res.Set(context.Background(), ws, k, v); err != nil {
		e.t.Fatal(err)
	}
}

// configureSMTP stores the ws-alpha SMTP secrets; footer adds OUTREACH_FOOTER (profile outreach_smtp).
func (e oactEnv) configureSMTP(footer bool) {
	e.t.Helper()
	for k, v := range map[string]string{"SMTP_HOST": "127.0.0.1", "SMTP_PORT": "25251", "SMTP_EMAIL": "sender@contract.example.com",
		"SMTP_PASSWORD": "ctk_outreach_smtp_password_0001", "SMTP_USE_TLS": "0"} {
		e.setSecret("ws-alpha", k, v)
	}
	if footer {
		e.setSecret("ws-alpha", "OUTREACH_FOOTER", "Contract Co, 1 Example Street, Springfield 00000")
	}
}

func (e oactEnv) status(id string) string {
	e.t.Helper()
	var s string
	if err := wcrudOwnerQueryRow(e.t, `SELECT status FROM outreach_sequences WHERE id = $1`, id).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e oactEnv) sendJobs() int {
	return e.rowCount(e.t, `SELECT count(*) FROM jobs WHERE type = 'send' AND workspace_id = 'ws-alpha'`)
}

func (e oactEnv) scheduleEnabled(id string) (enabled, present bool) {
	e.t.Helper()
	n := e.rowCount(e.t, `SELECT count(*) FROM outreach_schedules WHERE sequence_id = $1`, id)
	if n == 0 {
		return false, false
	}
	if err := wcrudOwnerQueryRow(e.t, `SELECT enabled FROM outreach_schedules WHERE sequence_id = $1`, id).Scan(&enabled); err != nil {
		e.t.Fatal(err)
	}
	return enabled, true
}

func TestStatsAndSendsMatchFixtures(t *testing.T) {
	e := newOactEnv(t, nil, config.OutreachSettings{})
	order := strings.Fields(`seq_stats_a01 seq_stats_a02 seq_stats_foreign_404 seq_stats_unknown_404 seq_sends_a01 seq_sends_a02_empty
		seq_sends_foreign_empty seq_sends_unknown_empty`)
	n := 0
	e.replay(t, order, func(string) bool { n++; return true })
	if n != len(order) {
		t.Fatalf("replayed %d of %d", n, len(order))
	}
}

func TestEnrollCountsMatchFixtures(t *testing.T) {
	e := newOactEnv(t, nil, config.OutreachSettings{})
	order := strings.Fields(`enroll_ok enroll_consent_source enroll_mixed_skips enroll_duplicate_ids_in_body enroll_empty_list
		enroll_unknown_seq_404 enroll_foreign_seq_404 enroll_missing_body_422 enroll_bad_ids_422 enroll_as_editor_403 enroll_no_auth_401
		seq_stats_a02_after_enroll`)
	n := 0
	e.replay(t, order, func(string) bool { n++; return true })
	if n != len(order) {
		t.Fatalf("replayed %d of %d", n, len(order))
	}
}

// TestActionFlowMatchesFixtures replays the default-profile recording top to bottom: SMTP unconfigured, configured without the
// footer, then execute and pause.
func TestActionFlowMatchesFixtures(t *testing.T) {
	e := newOactEnv(t, nil, config.OutreachSettings{})
	e.replay(t, strings.Fields(`start_smtp_not_configured_400 start_unknown_404 start_foreign_404 start_as_editor_403`), func(string) bool { return true })
	e.configureSMTP(false)
	e.replay(t, strings.Fields(`start_no_consent_400 start_no_footer_400 enroll_ok enroll_consent_source enroll_mixed_skips
		enroll_duplicate_ids_in_body enroll_empty_list seq_stats_a02_after_enroll seq_stats_a01_after_enroll execute_a01 execute_a01_again_dedups execute_a02 execute_unknown_404
		execute_foreign_404 execute_as_editor_403 pause_a01 pause_a01_again seq_get_a01_paused pause_unknown_404 pause_foreign_404
		pause_as_editor_403`), func(string) bool { return true })
}

func TestStartProfileMatchesFixtures(t *testing.T) {
	e := newOactEnv(t, nil, config.OutreachSettings{})
	e.configureSMTP(true)
	e.replay(t, strings.Fields(`start_ok start_ok_again start_seq_get_active start_execute_dedups_ticker start_no_consent_still_400
		start_pause_after`), func(string) bool { return true })
}

func TestStartChecksInOrder(t *testing.T) {
	e := newOactEnv(t, nil, config.OutreachSettings{})
	step := func(id string, code int, detail string) {
		t.Helper()
		c, m, raw := e.as("alice", "", "POST", "/api/outreach/sequences/"+id+"/start", "")
		if c != code || (detail != "" && m["detail"] != detail) {
			t.Fatalf("%s: %d %s, want %d %q", id, c, raw, code, detail)
		}
	}
	step("nope", 404, "Sequence not found") // 404 before the SMTP check
	step("seq-out-a-02", 400, "SMTP not configured")
	e.configureSMTP(false)
	step("seq-out-a-02", 400, "consent_basis required before activating")
	step("seq-out-a-01", 400, "OUTREACH_FOOTER (physical address) required")
	e.setSecret("ws-alpha", "OUTREACH_FOOTER", "   ") // blank after strip is still missing
	step("seq-out-a-01", 400, "OUTREACH_FOOTER (physical address) required")
	if s := e.status("seq-out-a-01"); s != "draft" {
		t.Fatalf("a failed check must not activate: %s", s)
	}
	ownerExec(t, `UPDATE outreach_sequences SET consent_basis = '  ' WHERE id = 'seq-out-a-01'`)
	e.setSecret("ws-alpha", "OUTREACH_FOOTER", "1 Main St")
	step("seq-out-a-01", 400, "consent_basis required before activating") // whitespace-only consent_basis
}

func TestStartActivatesAndTicks(t *testing.T) {
	e := newOactEnv(t, nil, config.OutreachSettings{})
	e.configureSMTP(true)
	c, m, raw := e.as("alice", "", "POST", "/api/outreach/sequences/seq-out-a-01/start", "")
	if c != 200 || m["status"] != "active" || len(m) != 1 {
		t.Fatalf("%d %s", c, raw)
	}
	if s := e.status("seq-out-a-01"); s != "active" {
		t.Fatalf("status %s", s)
	}
	if en, present := e.scheduleEnabled("seq-out-a-01"); !present || !en {
		t.Fatalf("schedule present=%v enabled=%v", present, en)
	}
	if n := e.sendJobs(); n == 0 {
		t.Fatal("the initial tick enqueued no send job for the due enrollments")
	}
}

// TestStartTickFailureStillActive: a failing initial tick is swallowed; the sequence stays active and the answer is 200.
func TestStartTickFailureStillActive(t *testing.T) {
	e := newOactEnv(t, nil, config.OutreachSettings{})
	e.configureSMTP(true)
	ownerExec(t, `CREATE OR REPLACE FUNCTION oact_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'oact tick failure'; END $$`)
	ownerExec(t, `CREATE TRIGGER oact_fail_schedule BEFORE INSERT OR UPDATE ON outreach_schedules FOR EACH ROW EXECUTE FUNCTION oact_fail()`)
	c, m, raw := e.as("alice", "", "POST", "/api/outreach/sequences/seq-out-a-01/start", "")
	if c != 200 || m["status"] != "active" {
		t.Fatalf("%d %s", c, raw)
	}
	if s := e.status("seq-out-a-01"); s != "active" {
		t.Fatalf("status %s", s)
	}
	if n := e.sendJobs(); n != 0 {
		t.Fatalf("a failed tick leaves no jobs (one transaction), got %d", n)
	}
	if strings.Contains(raw, "oact") {
		t.Fatalf("error text leaked: %s", raw)
	}
}

func TestPauseDisablesSchedule(t *testing.T) {
	e := newOactEnv(t, nil, config.OutreachSettings{})
	e.configureSMTP(true)
	e.as("alice", "", "POST", "/api/outreach/sequences/seq-out-a-01/start", "")
	if en, _ := e.scheduleEnabled("seq-out-a-01"); !en {
		t.Fatal("start did not enable the schedule")
	}
	c, m, raw := e.as("alice", "", "POST", "/api/outreach/sequences/seq-out-a-01/pause", "")
	if c != 200 || m["status"] != "paused" {
		t.Fatalf("%d %s", c, raw)
	}
	if s := e.status("seq-out-a-01"); s != "paused" {
		t.Fatalf("status %s", s)
	}
	if en, present := e.scheduleEnabled("seq-out-a-01"); !present || en {
		t.Fatalf("schedule present=%v enabled=%v after pause", present, en)
	}
	if c, m, _ := e.as("alice", "", "POST", "/api/outreach/sequences/nope/pause", ""); c != 404 || m["detail"] != "Sequence not found" {
		t.Fatalf("unknown: %d %v", c, m)
	}
}

func TestEnrollSkipReasons(t *testing.T) {
	e := newOactEnv(t, nil, config.OutreachSettings{})
	// 7: no email; 16: suppressed; 19: suppressed through the collapsed Gmail key; 20: ok (mixed case); 20 again: already enrolled; 999: unknown
	c, m, raw := e.as("alice", "", "POST", "/api/outreach/sequences/seq-out-a-02/enroll", `{"lead_ids":[7,16,19,20,20,999]}`)
	if c != 200 || m["enrolled"] != float64(1) || m["total_lead_ids"] != float64(6) {
		t.Fatalf("%d %s", c, raw)
	}
	var reasons []string
	for _, s := range m["skipped"].([]any) {
		o := s.(map[string]any)
		reasons = append(reasons, fmt.Sprintf("%v:%v", o["lead_id"], o["reason"]))
	}
	want := "7:no_email 16:suppressed 19:suppressed 20:already_enrolled 999:no_email"
	if got := strings.Join(reasons, " "); got != want {
		t.Fatalf("skips %q want %q", got, want)
	}
	var src, email string
	if err := wcrudOwnerQueryRow(t, `SELECT consent_source, to_email_snapshot FROM outreach_enrollments WHERE sequence_id='seq-out-a-02' AND lead_id=20`).Scan(&src, &email); err != nil {
		t.Fatal(err)
	}
	if src != "manual_enroll" || email != "mixed.case@example.com" {
		t.Fatalf("consent_source %q email %q", src, email)
	}
	e.as("alice", "", "POST", "/api/outreach/sequences/seq-out-a-02/enroll", `{"lead_ids":[1],"consent_source":"form_2026"}`)
	if err := wcrudOwnerQueryRow(t, `SELECT consent_source FROM outreach_enrollments WHERE sequence_id='seq-out-a-02' AND lead_id=1`).Scan(&src); err != nil || src != "form_2026" {
		t.Fatalf("consent_source %q %v", src, err)
	}
	// hardening: a consent_source past varchar(120) is a 422, not a 500
	c, _, raw = e.as("alice", "", "POST", "/api/outreach/sequences/seq-out-a-02/enroll", `{"lead_ids":[5],"consent_source":"`+strings.Repeat("x", 121)+`"}`)
	if c != 422 || !strings.Contains(raw, "string_too_long") {
		t.Fatalf("long consent_source: %d %s", c, raw)
	}
}

func TestEnrollTooManyIds422(t *testing.T) {
	e := newOactEnv(t, nil, config.OutreachSettings{})
	ids := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"lead_ids":[`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Itoa(50000 + i))
		}
		b.WriteString(`]}`)
		return b.String()
	}
	c, m, raw := e.as("alice", "", "POST", "/api/outreach/sequences/seq-out-a-02/enroll", ids(5001))
	if c != 422 || !strings.Contains(raw, `"too_long"`) || !strings.Contains(raw, `"lead_ids"`) {
		t.Fatalf("%d %s", c, raw)
	}
	if n := e.rowCount(t, `SELECT count(*) FROM outreach_enrollments WHERE sequence_id='seq-out-a-02'`); n != 0 {
		t.Fatalf("a refused request enrolled %d rows", n)
	}
	// exactly 5000 is accepted (all unknown ids: skipped as no_email, spread over ten transactions)
	c, m, raw = e.as("alice", "", "POST", "/api/outreach/sequences/seq-out-a-02/enroll", ids(5000))
	if c != 200 || m["total_lead_ids"] != float64(5000) || len(m["skipped"].([]any)) != 5000 {
		t.Fatalf("5000: %d %.200s", c, raw)
	}
}

func TestEnrollForeignLeadIsNoEmailSkip(t *testing.T) {
	e := newOactEnv(t, nil, config.OutreachSettings{})
	c, m, raw := e.as("alice", "", "POST", "/api/outreach/sequences/seq-out-a-02/enroll", `{"lead_ids":[3,17]}`) // ws-beta leads
	sk, _ := m["skipped"].([]any)
	if c != 200 || m["enrolled"] != float64(0) || len(sk) != 2 {
		t.Fatalf("%d %s", c, raw)
	}
	for _, s := range sk {
		if s.(map[string]any)["reason"] != "no_email" {
			t.Fatalf("%v", s)
		}
	}
	if n := e.rowCount(t, `SELECT count(*) FROM outreach_enrollments WHERE sequence_id='seq-out-a-02'`); n != 0 {
		t.Fatalf("enrolled a foreign lead: %d", n)
	}
	// out-of-int4 ids never reach the lead query (Python answers 500)
	c, m, raw = e.as("alice", "", "POST", "/api/outreach/sequences/seq-out-a-02/enroll", `{"lead_ids":[9999999999,-1]}`)
	if c != 200 || m["enrolled"] != float64(0) || len(m["skipped"].([]any)) != 2 {
		t.Fatalf("%d %s", c, raw)
	}
}

func TestExecuteEnqueuesOnce(t *testing.T) {
	e := newOactEnv(t, nil, config.OutreachSettings{})
	ownerExec(t, `UPDATE outreach_sequences SET status = 'active' WHERE id = 'seq-out-a-01'`)
	c, m, raw := e.as("alice", "", "POST", "/api/outreach/sequences/seq-out-a-01/execute", "")
	n, _ := m["enqueued"].(float64)
	if c != 200 || n < 1 || len(m) != 1 {
		t.Fatalf("%d %s", c, raw)
	}
	if got := e.sendJobs(); got != int(n) {
		t.Fatalf("jobs %d, enqueued %v", got, n)
	}
	if c, m, _ := e.as("alice", "", "POST", "/api/outreach/sequences/seq-out-a-01/execute", ""); c != 200 || m["enqueued"] != float64(0) {
		t.Fatalf("second execute: %d %v", c, m)
	}
	if got := e.rowCount(t, `SELECT count(*) FROM jobs WHERE type='send' AND workspace_id='ws-alpha' AND executor='python'`); got != int(n) {
		t.Fatalf("executor python jobs %d", got)
	}
}

func TestExecuteHonoursCohortAndMaxEnqueue(t *testing.T) {
	e := newOactEnv(t, map[string]string{"GO_EXECUTOR_ENABLED": "1", "GO_EXECUTOR_WORKSPACES": "ws-alpha"},
		config.OutreachSettings{GoExecutorSend: true, TickMaxEnqueue: 1})
	ownerExec(t, `UPDATE outreach_sequences SET status = 'active' WHERE id = 'seq-out-a-01'`)
	ownerExec(t, `UPDATE outreach_enrollments SET status='pending', next_send_at='2026-08-29 08:00:00+00' WHERE id IN (1,2)`)
	if c, m, raw := e.as("alice", "", "POST", "/api/outreach/sequences/seq-out-a-01/execute", ""); c != 200 || m["enqueued"] != float64(1) {
		t.Fatalf("%d %s", c, raw)
	}
	if got := e.rowCount(t, `SELECT count(*) FROM jobs WHERE type='send' AND workspace_id='ws-alpha' AND executor='go'`); got != 1 {
		t.Fatalf("go-executor jobs %d", got)
	}
}

func TestActionsRequireAdminExceptReads(t *testing.T) {
	e := newOactEnv(t, nil, config.OutreachSettings{})
	writes := []struct{ path, body string }{
		{"start", ""}, {"pause", ""}, {"enroll", `{"lead_ids":[1]}`}, {"execute", ""},
	}
	for _, who := range []struct{ name, ws string }{{"erin", ""}, {"bob", "ws-alpha"}} {
		for _, w := range writes {
			if c, _, raw := e.as(who.name, who.ws, "POST", "/api/outreach/sequences/seq-out-a-01/"+w.path, w.body); c != 403 {
				t.Errorf("%s %s: %d %s want 403", who.name, w.path, c, raw)
			}
		}
		for _, p := range []string{"stats", "sends"} {
			if c, _, raw := e.as(who.name, who.ws, "GET", "/api/outreach/sequences/seq-out-a-01/"+p, ""); c != 200 {
				t.Errorf("%s GET %s: %d %s want 200", who.name, p, c, raw)
			}
		}
	}
	// the role is checked before the body is validated
	if c, _, _ := e.as("erin", "", "POST", "/api/outreach/sequences/seq-out-a-01/enroll", `{"lead_ids":"x"}`); c != 403 {
		t.Errorf("editor with invalid body: %d", c)
	}
	for _, m := range [][2]string{{"POST", "start"}, {"POST", "pause"}, {"POST", "enroll"}, {"POST", "execute"}, {"GET", "stats"}, {"GET", "sends"}} {
		if c, _, _ := e.as("", "", m[0], "/api/outreach/sequences/seq-out-a-01/"+m[1], ""); c != 401 {
			t.Errorf("no auth %v: %d", m, c)
		}
	}
	if s := e.status("seq-out-a-01"); s != "draft" {
		t.Errorf("a refused request changed status: %s", s)
	}
}

func TestActionRoutesForeignWorkspace404(t *testing.T) {
	e := newOactEnv(t, nil, config.OutreachSettings{})
	e.configureSMTP(true)
	for _, c := range [][3]string{{"POST", "start", ""}, {"POST", "pause", ""}, {"POST", "enroll", `{"lead_ids":[1]}`}, {"POST", "execute", ""}, {"GET", "stats", ""}} {
		code, m, raw := e.as("alice", "", c[0], "/api/outreach/sequences/seq-out-b-01/"+c[1], c[2])
		if code != 404 || m["detail"] != "Sequence not found" {
			t.Errorf("%v: %d %s", c, code, raw)
		}
	}
	// sends has no ownership check (as Python): a foreign id is an empty list
	if code, m, raw := e.as("alice", "", "GET", "/api/outreach/sequences/seq-out-b-01/sends", ""); code != 200 || len(m["sends"].([]any)) != 0 {
		t.Errorf("sends: %d %s", code, raw)
	}
	if s := e.status("seq-out-b-01"); s != "draft" {
		t.Errorf("foreign sequence changed: %s", s)
	}
}
