//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

type pubEnv struct {
	oseqEnv
	pub http.Handler
}

func pubPurge() {
	oseqPurge()
	for _, q := range []string{`DELETE FROM outreach_suppressions WHERE workspace_id IN ('ws-alpha','ws-beta')`,
		`DELETE FROM outreach_inbound_messages WHERE workspace_id IN ('ws-alpha','ws-beta')`,
		`DELETE FROM governance_audit_events WHERE route LIKE '%unsubscribe%' OR route LIKE '%webhooks/bounce%'`} {
		wcrudOwnerExecQuiet(q)
	}
}

// newPubEnv seeds the recorded outreach rows the public routes touch: the three sequences, the ws-beta enrollments 11-13 with
// the bounce-webhook target sends, one ws-alpha send, the suppression rows and the pre-seeded ledger row of the duplicate case.
func newPubEnv(t *testing.T, opt pubOpts) pubEnv {
	t.Helper()
	e := newLeadsEnv(t)
	e.seed(t, "erin", pythonHash, true, "user", false)
	e.member(t, "ws-alpha", e.mustUserByName(t, "erin").ID, "editor")
	pubPurge()
	t.Cleanup(pubPurge)
	ownerExec(t, `INSERT INTO outreach_sequences (id, workspace_id, name, description, steps, status, daily_limit, send_window_start,
		send_window_end, send_window_tz, consent_basis, bounce_count, complaint_count, auto_paused, created_at, updated_at) VALUES
		('seq-out-a-01','ws-alpha','Three step cold outreach','primary contract sequence',CAST($1 AS json),'draft',50,9,18,'UTC','legitimate_interest',0,0,false,'2026-08-29 08:00:00+00','2026-08-29 08:00:00+00'),
		('seq-out-b-01','ws-beta','Beta sequence','beta tenant',CAST('[]' AS json),'draft',50,9,18,'UTC','consent_contract',0,0,false,'2026-08-29 08:30:00+00','2026-08-29 08:30:00+00')`, oseqStepsA1)
	for _, en := range [][]any{{11, "ws-beta", "seq-out-b-01", 17, "beta@zenithrobotics.example.com"}, {12, "ws-beta", "seq-out-b-01", 18, "soft-target@contract.example.com"},
		{13, "ws-beta", "seq-out-b-01", 3, "complaint-target@contract.example.com"}, {1, "ws-alpha", "seq-out-a-01", 1, "hot@alpha.example.com"}} {
		ownerExec(t, `INSERT INTO outreach_enrollments (id, workspace_id, sequence_id, lead_id, to_email_snapshot, consent_source, consent_at, current_step, status)
			VALUES ($1,$2,$3,$4,$5,'manual_enroll','2026-08-29 07:30:00+00',0,'sent')`, en...)
	}
	for _, s := range [][]any{
		{12, "ws-beta", "seq-out-b-01", 11, 17, 0, "beta@zenithrobotics.example.com", "<hook-hard-b@contract.example.com>"},
		{13, "ws-beta", "seq-out-b-01", 12, 18, 0, "soft-target@contract.example.com", "<hook-soft-1-b@contract.example.com>"},
		{14, "ws-beta", "seq-out-b-01", 12, 18, 1, "soft-target@contract.example.com", "<hook-soft-2-b@contract.example.com>"},
		{15, "ws-beta", "seq-out-b-01", 12, 18, 2, "soft-target@contract.example.com", "<hook-soft-3-b@contract.example.com>"},
		{16, "ws-beta", "seq-out-b-01", 13, 3, 0, "complaint-target@contract.example.com", "<hook-complaint-b@contract.example.com>"},
		{17, "ws-beta", "seq-out-b-01", 11, 17, 1, "beta@zenithrobotics.example.com", "<hook-dup-b@contract.example.com>"},
		{1, "ws-alpha", "seq-out-a-01", 1, 1, 0, "hot@alpha.example.com", "<hook-hard-a@contract.example.com>"},
		{2, "ws-alpha", "seq-out-a-01", 1, 1, 1, "beta@zenithrobotics.example.com", "<hook-dup-b@contract.example.com>"},
	} {
		s = append(s, "idem:"+s[1].(string)+":"+strconv.Itoa(s[0].(int)))
		ownerExec(t, `INSERT INTO outreach_sends (id, workspace_id, sequence_id, enrollment_id, lead_id, step_number, to_email, subject, status, message_id,
			idempotency_key, charged_usd, migrated, error, sent_at, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'subject','sent',$8,$9,0,false,'','2026-08-29 08:45:00+00','2026-08-29 08:45:00+00','2026-08-29 08:45:00+00')`, s...)
	}
	for _, r := range oactSuppress {
		ownerExec(t, `INSERT INTO outreach_suppressions (workspace_id, email, reason, source, locked, created_at) VALUES ($1,$2,$3,$4,$5,$6::timestamptz)`, r...)
	}
	ownerExec(t, `INSERT INTO outreach_inbound_messages (workspace_id, imap_uid, uidvalidity, source_message_id, matched_send_id, kind, recipient, diagnostic, processed_at)
		VALUES ('ws-beta','webhook','webhook','<hook-dup-b@contract.example.com>',17,'hard','beta@zenithrobotics.example.com','webhook','2026-08-29 08:58:00+00')`)
	ownerExec(t, `SELECT setval(pg_get_serial_sequence('outreach_sends','id'), GREATEST(1000, (SELECT max(id) FROM outreach_sends)))`)
	ownerExec(t, `SELECT setval(pg_get_serial_sequence('outreach_enrollments','id'), GREATEST(1000, (SELECT max(id) FROM outreach_enrollments)))`)

	d := pubDeps(opt)
	d.Pool, d.Signer, d.Identity = e.pool, testSigner, e.st
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(e.pool, nil))
	registerOutreachSuppressions(r, d)
	registerOutreachPublic(r, d)
	e.h = r.Handler()
	return pubEnv{oseqEnv{e}, e.h}
}

func (e pubEnv) one(t *testing.T, q string, args ...any) string {
	t.Helper()
	var s string
	if err := wcrudOwnerQueryRow(t, q, args...).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// supp lists a workspace's suppression rows as email|reason|source|L (locked) or - (sorted).
func (e pubEnv) supp(t *testing.T, ws string) string {
	return e.one(t, `SELECT COALESCE(string_agg(email||'|'||reason||'|'||source||'|'||CASE WHEN locked THEN 'L' ELSE '-' END, E'\n' ORDER BY email), '')
		FROM outreach_suppressions WHERE workspace_id=$1`, ws)
}

func (e pubEnv) post(tokenPath, addr string) *httptest.ResponseRecorder {
	return pubReq{method: "POST", path: tokenPath, addr: addr}.do(e.pub)
}

func jsonEq(t *testing.T, got string, want json.RawMessage) bool {
	t.Helper()
	var g, w any
	_ = json.Unmarshal([]byte(got), &g)
	_ = json.Unmarshal(want, &w)
	return toJSON(g) == toJSON(w)
}

// TestUnsubscribePostAcceptsPythonToken replays the recorded one-click POSTs from one client in the recorded order (the
// 21st and 22nd are rate limited) and checks what each stored.
func TestUnsubscribePostAcceptsPythonToken(t *testing.T) {
	e := newPubEnv(t, pubOpts{})
	order := strings.Fields(`unsub_post_empty_400 unsub_post_forged_400 unsub_post_expired_400 unsub_post_no_dot_400 unsub_post_valid
		unsub_post_valid_again unsub_post_valid_nonascii unsub_post_valid_html_email unsub_post_valid_beta
		unsub_post_burst_01 unsub_post_burst_02 unsub_post_burst_03 unsub_post_burst_04 unsub_post_burst_05 unsub_post_burst_06
		unsub_post_burst_07 unsub_post_burst_08 unsub_post_burst_09 unsub_post_burst_10 unsub_post_burst_11
		unsub_post_21st_429 unsub_post_22nd_429`)
	for _, name := range order {
		fx := contract.Load(t, "outreach/"+name)
		rec := e.post(fx.Request.Path, "192.0.2.77:1")
		if rec.Code != fx.Response.Status || !jsonEq(t, rec.Body.String(), fx.Response.Body) {
			t.Fatalf("%s: %d %q, want %d %s", name, rec.Code, rec.Body.String(), fx.Response.Status, fx.Response.Body)
		}
	}
	wantA := []string{`a<b>&"x"@example.com|unsubscribe|unsubscribe|L`, "bounced-user@example.com|bounce|seq-out-a-01|-",
		"jörg@xn--exmple-cua.de|unsubscribe|seq-out-a-01|L", "locked-complaint@example.com|complaint|inbound|L", "locked-unsub@example.com|unsubscribe|seq-out-a-01|L",
		"manual-removable@example.com|manual|manual|-", "patlee@gmail.com|manual|manual|-", "+cmd@sheet.example.com|manual|manual|-",
		"unsub-valid@example.com|unsubscribe|seq-out-a-01|L"}
	if got := e.supp(t, "ws-alpha"); strings.Count(got, "\n")+1 != len(wantA) {
		t.Fatalf("alpha rows:\n%s", got)
	}
	for _, w := range wantA {
		if !strings.Contains(e.supp(t, "ws-alpha"), w) {
			t.Errorf("missing %q in\n%s", w, e.supp(t, "ws-alpha"))
		}
	}
	if got := e.supp(t, "ws-beta"); !strings.Contains(got, "unsub-beta@example.com|unsubscribe|seq-out-b-01|L") || strings.Contains(got, "unsub-valid") {
		t.Fatalf("beta rows:\n%s", got)
	}
}

func (e pubEnv) unsubToken(t *testing.T, ws, email, seq string) string {
	return "/api/outreach/unsubscribe?token=" + pubToken(t, ws, email, seq, 30)
}

func TestUnsubscribeIdempotentAndLocked(t *testing.T) {
	e := newPubEnv(t, pubOpts{})
	path := e.unsubToken(t, "ws-alpha", "Idem@Example.COM", "seq-out-a-01")
	for i := 0; i < 3; i++ {
		if rec := e.post(path, "192.0.2.1:1"); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"unsubscribed"}` {
			t.Fatalf("call %d: %d %q", i, rec.Code, rec.Body.String())
		}
	}
	if n := e.one(t, `SELECT count(*)::text FROM outreach_suppressions WHERE email='idem@example.com'`); n != "1" {
		t.Fatalf("rows %s", n)
	}
	// source is the sequence, or "unsubscribe" without one; gmail variants are stored collapsed like every other write
	e.post(e.unsubToken(t, "ws-alpha", "A.b+x@Gmail.com", ""), "192.0.2.2:1")
	if got := e.supp(t, "ws-alpha"); !strings.Contains(got, "ab@gmail.com|unsubscribe|unsubscribe|L") || !strings.Contains(got, "idem@example.com|unsubscribe|seq-out-a-01|L") {
		t.Fatalf("rows:\n%s", got)
	}
	// locked: the admin route refuses to remove it
	c, m, raw := e.as("alice", "", "DELETE", "/api/outreach/suppressions/idem%40example.com", "")
	if c != 403 || m["detail"] != "Cannot remove an unsubscribe/complaint suppression" {
		t.Fatalf("%d %s", c, raw)
	}
	if n := e.one(t, `SELECT count(*)::text FROM outreach_suppressions WHERE email='idem@example.com'`); n != "1" {
		t.Fatal("locked row removed")
	}
	// the stored row blocks a send to any spelling
	var blocked bool
	_ = tenancy.InWorkspace(context.Background(), e.pool, "ws-alpha", func(tx pgx.Tx) (err error) {
		blocked, err = outreachstore.IsSuppressed(context.Background(), tx, "ws-alpha", "IDEM@example.com")
		return err
	})
	if !blocked {
		t.Fatal("not suppressed")
	}
}

// TestUnsubscribeNoAuthNoCSRF: one-click needs no login, no CSRF token and no cookie; the workspace is the token's, whatever
// the caller sends.
func TestUnsubscribeNoAuthNoCSRF(t *testing.T) {
	e := newPubEnv(t, pubOpts{})
	path := e.unsubToken(t, "ws-beta", "noauth@example.com", "seq-out-b-01")
	req := pubReq{method: "POST", path: path, addr: "192.0.2.3:1", hdr: map[string]string{
		"Origin": "https://evil.example", "Referer": "https://evil.example/x", "Content-Type": "application/x-www-form-urlencoded",
		"X-Workspace-Id": "ws-alpha", "Authorization": "Bearer not-a-jwt", "Cookie": "session=junk"}, body: "List-Unsubscribe=One-Click"}
	rec := req.do(e.pub)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"unsubscribed"}` {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(e.supp(t, "ws-beta"), "noauth@example.com|unsubscribe|seq-out-b-01|L") || strings.Contains(e.supp(t, "ws-alpha"), "noauth") {
		t.Fatal("the row must land in the token's workspace only")
	}
	// the landing page is public too
	if rec := (pubReq{method: "GET", path: path, addr: "192.0.2.3:1"}).do(e.pub); rec.Code != 200 {
		t.Fatalf("landing %d", rec.Code)
	}
}

// TestPublicRoutesAuditNoToken: the audit trail never stores a token or an address from the public routes.
func TestPublicRoutesAuditNoToken(t *testing.T) {
	e := newPubEnv(t, pubOpts{})
	tok := pubToken(t, "ws-alpha", "audit-leak@example.com", "", 30)
	e.post("/api/outreach/unsubscribe?token="+tok, "192.0.2.4:1")
	pubReq{method: "GET", path: "/api/outreach/unsubscribe?token=" + tok}.do(e.pub)
	time.Sleep(500 * time.Millisecond)
	if n := e.one(t, `SELECT count(*)::text FROM governance_audit_events WHERE to_jsonb(governance_audit_events)::text LIKE '%'||$1||'%' OR to_jsonb(governance_audit_events)::text LIKE '%audit-leak%'`, tok[:20]); n != "0" {
		t.Fatalf("%s audit rows carry the token or address", n)
	}
}

// --- bounce webhook ---

func (e pubEnv) hook(body string, secret string) *httptest.ResponseRecorder {
	h := map[string]string{}
	if secret != "" {
		h["X-Webhook-Secret"] = secret
	}
	return pubReq{method: "POST", path: "/api/outreach/webhooks/bounce", body: body, hdr: h}.do(e.pub)
}

func whBody(id string, extra string) string {
	return `{"message_id":"` + id + `"` + extra + `}`
}

func TestWebhookSecretOrderLookup(t *testing.T) {
	e := newPubEnv(t, pubOpts{webhookToken: pubWebhookSecret})
	known := whBody("<hook-hard-b@contract.example.com>", "")
	for _, s := range []string{"", "wrong-secret", pubWebhookSecret + " "} {
		if rec := e.hook(known, s); rec.Code != 403 {
			t.Fatalf("%q: %d", s, rec.Code)
		}
	}
	if n := e.one(t, `SELECT count(*)::text FROM outreach_inbound_messages WHERE workspace_id='ws-beta'`); n != "1" {
		t.Fatalf("a refused call wrote %s ledger rows", n)
	}
	for _, id := range []string{"<nope@contract.example.com>", " ", "", "<hook-hard-b@contract.example.com> x"} {
		rec := e.hook(whBody(id, ""), pubWebhookSecret)
		if id == "" || id == " " {
			if rec.Code != 404 {
				t.Fatalf("%q: %d %s", id, rec.Code, rec.Body.String())
			}
			continue
		}
		if rec.Code != 404 || strings.TrimSpace(rec.Body.String()) != `{"detail":"message not found"}` {
			t.Fatalf("%q: %d %s", id, rec.Code, rec.Body.String())
		}
	}
	if n := e.one(t, `SELECT count(*)::text FROM outreach_inbound_messages`); n != "1" {
		t.Fatalf("%s ledger rows", n)
	}
}

// TestWebhookKindsApplied replays the recorded webhook cases in order and checks the effects the after-state fixtures
// recorded: hard, three soft (the third reaches the max), complaint and the pre-seeded ledger duplicate.
func TestWebhookKindsApplied(t *testing.T) {
	e := newPubEnv(t, pubOpts{webhookToken: pubWebhookSecret})
	for _, name := range strings.Fields(`wh_hard wh_hard_replay_duplicate wh_soft_1 wh_soft_2 wh_soft_3_reaches_max wh_complaint wh_preseeded_ledger_duplicate`) {
		fx := contract.Load(t, "outreach/"+name)
		rec := e.hook(string(fx.Request.Body), pubWebhookSecret)
		if rec.Code != fx.Response.Status || !jsonEq(t, rec.Body.String(), fx.Response.Body) {
			t.Fatalf("%s: %d %q, want %d %s", name, rec.Code, rec.Body.String(), fx.Response.Status, fx.Response.Body)
		}
	}
	want := "beta-suppressed@example.com|manual|manual|-\nbeta@zenithrobotics.example.com|bounce|webhook|-\n" +
		"complaint-target@contract.example.com|complaint|webhook|L\nsoft-target@contract.example.com|bounce|webhook|-"
	if got := e.supp(t, "ws-beta"); got != want {
		t.Fatalf("beta suppressions:\n%s\nwant\n%s", got, want)
	}
	if got := e.one(t, `SELECT bounce_count||','||complaint_count||','||auto_paused FROM outreach_sequences WHERE id='seq-out-b-01'`); got != "1,1,false" {
		t.Fatalf("counters %s", got)
	}
	if got := e.one(t, `SELECT string_agg(message_id||'='||status||'/'||error, ' ' ORDER BY id) FROM outreach_sends WHERE workspace_id='ws-beta'`); got !=
		"<hook-hard-b@contract.example.com>=bounced/webhook <hook-soft-1-b@contract.example.com>=sent/ <hook-soft-2-b@contract.example.com>=sent/ "+
			"<hook-soft-3-b@contract.example.com>=bounced/webhook <hook-complaint-b@contract.example.com>=bounced/webhook <hook-dup-b@contract.example.com>=sent/" {
		t.Fatalf("sends: %s", got)
	}
	if got := e.one(t, `SELECT string_agg(id||'='||status||'/'||soft_bounce_count, ' ' ORDER BY id) FROM outreach_enrollments WHERE workspace_id='ws-beta'`); got != "11=bounced/0 12=sent/3 13=bounced/0" {
		t.Fatalf("enrollments: %s", got)
	}
	if got := e.one(t, `SELECT string_agg(source_message_id||'='||kind||'/'||imap_uid||'/'||uidvalidity||'/'||diagnostic, ' ' ORDER BY id) FROM outreach_inbound_messages WHERE workspace_id='ws-beta'`); !strings.Contains(got,
		"<hook-hard-b@contract.example.com>=hard/<hook-hard-b@contract.example.com>/webhook/webhook") || !strings.Contains(got, "<hook-complaint-b@contract.example.com>=complaint/") ||
		!strings.Contains(got, "<hook-soft-1-b@contract.example.com>=soft/") {
		t.Fatalf("ledger: %s", got)
	}
}

func TestWebhookReplayDuplicate(t *testing.T) {
	e := newPubEnv(t, pubOpts{webhookToken: pubWebhookSecret})
	body := whBody("<hook-hard-b@contract.example.com>", "")
	if rec := e.hook(body, pubWebhookSecret); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.hook(body, pubWebhookSecret); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"duplicate"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if got := e.one(t, `SELECT bounce_count::text FROM outreach_sequences WHERE id='seq-out-b-01'`); got != "1" {
		t.Fatalf("a replay must not double count: %s", got)
	}
	// the same message id replayed as a complaint is still a duplicate (the ledger key is the message id)
	if rec := e.hook(whBody("<hook-hard-b@contract.example.com>", `,"event":"complaint"`), pubWebhookSecret); strings.TrimSpace(rec.Body.String()) != `{"status":"duplicate"}` {
		t.Fatalf("%s", rec.Body.String())
	}
	// concurrent first deliveries: exactly one applies
	var wg sync.WaitGroup
	res := make([]string, 8)
	for i := range res {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res[i] = strings.TrimSpace(e.hook(whBody("<hook-complaint-b@contract.example.com>", `,"event":"complaint"`), pubWebhookSecret).Body.String())
		}()
	}
	wg.Wait()
	ok, dup := 0, 0
	for _, r := range res {
		switch r {
		case `{"status":"ok"}`:
			ok++
		case `{"status":"duplicate"}`:
			dup++
		}
	}
	if ok != 1 || dup != 7 {
		t.Fatalf("ok=%d dup=%d %v", ok, dup, res)
	}
	if got := e.one(t, `SELECT complaint_count::text FROM outreach_sequences WHERE id='seq-out-b-01'`); got != "1" {
		t.Fatalf("complaint_count %s", got)
	}
}

// TestWebhookCrossTenantIsolation: the workspace comes from the stored send, never from the caller: a workspace id in the
// body or a header cannot redirect the effects, and a ws-beta event leaves ws-alpha untouched (and vice versa).
func TestWebhookCrossTenantIsolation(t *testing.T) {
	e := newPubEnv(t, pubOpts{webhookToken: pubWebhookSecret})
	before := e.supp(t, "ws-alpha")
	rec := pubReq{method: "POST", path: "/api/outreach/webhooks/bounce", body: whBody("<hook-hard-b@contract.example.com>", `,"workspace_id":"ws-alpha"`),
		hdr: map[string]string{"X-Webhook-Secret": pubWebhookSecret, "X-Workspace-Id": "ws-alpha"}}.do(e.pub)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if e.supp(t, "ws-alpha") != before || e.one(t, `SELECT count(*)::text FROM outreach_inbound_messages WHERE workspace_id='ws-alpha'`) != "0" ||
		e.one(t, `SELECT bounce_count::text FROM outreach_sequences WHERE id='seq-out-a-01'`) != "0" {
		t.Fatal("ws-alpha was touched by a ws-beta event")
	}
	if !strings.Contains(e.supp(t, "ws-beta"), "beta@zenithrobotics.example.com|bounce|webhook|-") {
		t.Fatal("ws-beta was not updated")
	}
	// the other direction
	if rec := e.hook(whBody("<hook-hard-a@contract.example.com>", ""), pubWebhookSecret); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	if !strings.Contains(e.supp(t, "ws-alpha"), "hot@alpha.example.com|bounce|webhook|-") || strings.Contains(e.supp(t, "ws-beta"), "hot@alpha") {
		t.Fatal("alpha event landed wrongly")
	}
	// a message id present in two workspaces resolves to the newest send (Python order_by id desc): ws-alpha id 2 is
	// newer than ws-beta id 17 for <hook-dup-b@...>? No: 17 is newer, so the event belongs to ws-beta.
	if rec := e.hook(whBody("<hook-dup-b@contract.example.com>", ""), pubWebhookSecret); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"duplicate"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// TestWebhookNeverReadsOtherTenantRows: the route runs as the RLS-bound application role. Outside a workspace the role sees
// no outreach rows at all; the only cross-tenant read is the SECURITY DEFINER lookup, which returns the routing tuple.
func TestWebhookNeverReadsOtherTenantRows(t *testing.T) {
	e := newPubEnv(t, pubOpts{webhookToken: pubWebhookSecret})
	ctx := context.Background()
	for _, tbl := range []string{"outreach_sends", "outreach_suppressions", "outreach_inbound_messages", "outreach_enrollments"} {
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s visible without a workspace: %d %v", tbl, n, err)
		}
	}
	var super, bypass bool
	if err := e.pool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil || super || bypass {
		t.Fatalf("the application role must not bypass RLS: %v %v %v", super, bypass, err)
	}
	ref, err := outreachstore.LookupSendAnyWorkspace(ctx, e.pool, "<hook-soft-2-b@contract.example.com>")
	if err != nil || ref == nil || ref.WorkspaceID != "ws-beta" || ref.SendID != 14 || ref.ToEmail != "soft-target@contract.example.com" {
		t.Fatalf("%+v %v", ref, err)
	}
	// a soft event on a ws-beta message with the same recipient as an alpha suppression changes nothing in alpha
	before := e.supp(t, "ws-alpha")
	for i := 1; i <= 3; i++ {
		e.hook(whBody("<hook-soft-"+strconv.Itoa(i)+"-b@contract.example.com>", `,"hard":false`), pubWebhookSecret)
	}
	if e.supp(t, "ws-alpha") != before {
		t.Fatal("alpha rows changed")
	}
}

// TestWebhookLongMessageID: the ledger's imap_uid is 64 characters wide; a longer message id is keyed by its digest
// instead of failing the insert, and the replay is still a duplicate.
func TestWebhookLongMessageID(t *testing.T) {
	e := newPubEnv(t, pubOpts{webhookToken: pubWebhookSecret})
	long := "<" + strings.Repeat("a", 120) + "@contract.example.com>"
	ownerExec(t, `INSERT INTO outreach_sends (workspace_id, sequence_id, enrollment_id, lead_id, step_number, to_email, subject, status, message_id, idempotency_key, charged_usd, migrated, error)
		VALUES ('ws-beta','seq-out-b-01',13,3,5,'long@contract.example.com','s','sent',$1,'idem-long',0,false,'')`, long)
	for i, want := range []string{`{"status":"ok"}`, `{"status":"duplicate"}`} {
		if rec := e.hook(whBody(long, ""), pubWebhookSecret); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != want {
			t.Fatalf("call %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
}

// TestWebhookKindMapping: event=complaint wins over hard=false; hard=false is soft; any other event with the default
// hard=true is a hard bounce; hard accepts pydantic's lax booleans.
func TestWebhookKindMapping(t *testing.T) {
	e := newPubEnv(t, pubOpts{webhookToken: pubWebhookSecret})
	for _, c := range []struct{ id, extra, kind string }{
		{"<hook-hard-b@contract.example.com>", `,"event":"weird"`, "hard"},
		{"<hook-soft-1-b@contract.example.com>", `,"event":"bounce","hard":"no"`, "soft"},
		{"<hook-complaint-b@contract.example.com>", `,"event":"complaint","hard":false`, "complaint"},
	} {
		if rec := e.hook(whBody(c.id, c.extra), pubWebhookSecret); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
			t.Fatalf("%s: %d %s", c.extra, rec.Code, rec.Body.String())
		}
		if got := e.one(t, `SELECT kind FROM outreach_inbound_messages WHERE source_message_id=$1`, c.id); got != c.kind {
			t.Fatalf("%s: kind %s, want %s", c.extra, got, c.kind)
		}
	}
}
