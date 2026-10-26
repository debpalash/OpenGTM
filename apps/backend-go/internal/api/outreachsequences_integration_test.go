//go:build integration

package api

import (
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// oseqOrder is the recorded order of the sequence CRUD fixtures (contract_cases/outreach.yaml). State mutates
// top to bottom, so every replay runs the whole sequence.
var oseqOrder = strings.Fields(`seq_list seq_list_beta_bob seq_list_viewer_bob_alpha seq_list_no_auth seq_get_a01_stats
seq_get_a02 seq_get_beta_own seq_get_foreign_404 seq_get_unknown_404 seq_create_defaults seq_create_with_steps
seq_create_missing_name_422 seq_create_bad_step_422 seq_create_bad_limit_422 seq_create_as_editor_403 seq_create_as_viewer_403
seq_create_no_auth_401 seq_create_beta_bob seq_get_created seq_list_after_create seq_put_steps seq_put_all_fields
seq_put_status_paused seq_put_status_active seq_put_empty_body seq_put_bad_type_422 seq_get_after_put seq_put_unknown_404
seq_put_foreign_404 seq_put_as_editor_403 seq_delete_as_editor_403 seq_delete_ok seq_delete_again_404 seq_delete_unknown_404
seq_delete_foreign_404 seq_get_deleted_404`)

func oseqActor(name string) (who, ws string) {
	switch name {
	case "seq_list_beta_bob", "seq_get_beta_own", "seq_create_beta_bob":
		return "bob", "ws-beta"
	case "seq_list_viewer_bob_alpha", "seq_create_as_viewer_403":
		return "bob", "ws-alpha"
	case "seq_create_as_editor_403", "seq_put_as_editor_403", "seq_delete_as_editor_403":
		return "erin", ""
	}
	if strings.HasSuffix(name, "no_auth") || strings.HasSuffix(name, "no_auth_401") {
		return "", ""
	}
	return "alice", ""
}

type oseqEnv struct{ leadsEnv }

func oseqPurge() {
	for _, q := range []string{`DELETE FROM outreach_sends WHERE workspace_id IN ('ws-alpha','ws-beta')`,
		`DELETE FROM outreach_enrollments WHERE workspace_id IN ('ws-alpha','ws-beta')`,
		`DELETE FROM outreach_schedules WHERE workspace_id IN ('ws-alpha','ws-beta')`,
		`DELETE FROM outreach_sequences WHERE workspace_id IN ('ws-alpha','ws-beta')`} {
		wcrudOwnerExecQuiet(q)
	}
}

const oseqStepsA1 = `[{"step_number": 0, "subject": "Hi {{name}}", "body_html": "<p>Hello {{name}} at {{company}}.</p>", "delay_hours": 0},
{"step_number": 1, "subject": "Following up, {{name}}", "body_html": "<p>Any thoughts on {{company}}?</p>", "delay_hours": 72},
{"step_number": 2, "subject": "Last note", "body_html": "<p>Closing the loop.</p>", "delay_hours": 120}]`

// newOseqEnv seeds the contract users (erin editor in ws-alpha), the three recorded sequences, the ten ws-alpha
// enrollments and the sent rows behind seq-out-a-01's stats, then serves the sequence routes.
func newOseqEnv(t *testing.T) oseqEnv {
	t.Helper()
	e := newLeadsEnv(t)
	e.seed(t, "erin", pythonHash, true, "user", false)
	e.member(t, "ws-alpha", e.mustUserByName(t, "erin").ID, "editor")
	oseqPurge()
	t.Cleanup(oseqPurge)
	ownerExec(t, `INSERT INTO outreach_sequences (id, workspace_id, name, description, steps, status, daily_limit, send_window_start,
		send_window_end, send_window_tz, consent_basis, bounce_count, complaint_count, auto_paused, created_at, updated_at) VALUES
		('seq-out-a-01','ws-alpha','Three step cold outreach','primary contract sequence',CAST($1 AS json),'draft',50,9,18,'UTC','legitimate_interest',2,0,false,'2026-08-29 08:00:00+00','2026-08-29 08:00:00+00'),
		('seq-out-a-02','ws-alpha','Draft without consent','',CAST('[]' AS json),'draft',25,8,17,'Europe/Berlin','',0,0,false,'2026-08-29 07:00:00+00','2026-08-29 07:00:00+00'),
		('seq-out-b-01','ws-beta','Beta sequence','beta tenant',CAST('[{"step_number": 0, "subject": "Beta intro", "body_html": "<p>Hello from beta.</p>", "delay_hours": 0}]' AS json),'draft',50,9,18,'UTC','consent_contract',0,0,false,'2026-08-29 08:30:00+00','2026-08-29 08:30:00+00')`, oseqStepsA1)
	for i, st := range []string{"pending", "scheduled", "sent", "opened", "replied", "bounced", "failed", "suppressed", "completed", "skipped"} {
		ownerExec(t, `INSERT INTO outreach_enrollments (id, workspace_id, sequence_id, lead_id, to_email_snapshot, consent_source, consent_at, current_step, status)
			VALUES ($1,'ws-alpha','seq-out-a-01',$2,$4,'manual_enroll','2026-08-29 07:30:00+00',0,$3)`, 900000+i, i+1, st, "x"+strconv.Itoa(i)+"@contract.example.com")
	}
	for i := 0; i < 6; i++ {
		ownerExec(t, `INSERT INTO outreach_sends (workspace_id, sequence_id, enrollment_id, lead_id, step_number, to_email, subject, status, idempotency_key, charged_usd, migrated, error)
			VALUES ('ws-alpha','seq-out-a-01',$1,$2,0,'x@contract.example.com','s','sent',$3,0,false,'')`, 900000+i, i+1, "oseq-"+strconv.Itoa(i))
	}
	for i := 0; i < 3; i++ { // ws-beta: three scheduled enrollments, six sent rows
		ownerExec(t, `INSERT INTO outreach_enrollments (id, workspace_id, sequence_id, lead_id, to_email_snapshot, consent_source, consent_at, current_step, status)
			VALUES ($1,'ws-beta','seq-out-b-01',$2,$3,'manual_enroll','2026-08-29 07:30:00+00',0,'scheduled')`, 910000+i, 17+i, "b"+strconv.Itoa(i)+"@contract.example.com")
	}
	for i := 0; i < 6; i++ {
		ownerExec(t, `INSERT INTO outreach_sends (workspace_id, sequence_id, enrollment_id, lead_id, step_number, to_email, subject, status, idempotency_key, charged_usd, migrated, error)
			VALUES ('ws-beta','seq-out-b-01',$1,17,$2,'b@contract.example.com','s','sent',$3,0,false,'')`, 910000, i, "oseqb-"+strconv.Itoa(i))
	}
	d := Deps{Pool: e.pool, Signer: testSigner, Identity: e.st, Flags: config.Flags{PGLeadStore: true, Outreach: config.OutreachSettings{GoRoutesEnabled: true}}}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(e.pool, nil))
	registerOutreachSequences(r, d)
	e.h = r.Handler()
	return oseqEnv{e}
}

func (e oseqEnv) call(who, ws, method, path, body string) *httptest.ResponseRecorder {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
	}
	if ws != "" {
		hdr["X-Workspace-Id"] = ws
	}
	ct := ""
	if body != "" {
		ct = "application/json"
	}
	rec, _ := e.do(method, path, ct, body, hdr)
	return rec
}

var (
	oseqNormUUID = regexp.MustCompile(`00000000-0000-4000-8000-\d{12}`)
	oseqRealUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
)

// oseqDeviations are the recorded cases where Go answers differently from FastAPI on purpose (D3).
var oseqDeviations = map[string]int{"seq_put_status_active": 422}

// normalize rewrites volatile values of got (generated ids, timestamps) to the recorded sample where the
// fixture has one, keeping JSON types.
func oseqNormalize(got, want any, ids map[string]string) any {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return got
		}
		if id, _ := w["id"].(string); oseqNormUUID.MatchString(id) { // a row created during the run: its timestamps are volatile
			for _, k := range []string{"created_at", "updated_at"} {
				if _, ok := g[k].(string); ok {
					if wv, ok := w[k].(string); ok {
						g[k] = wv
					}
				}
			}
		}
		for k, wv := range w {
			if gv, ok := g[k]; ok {
				g[k] = oseqNormalize(gv, wv, ids)
			}
		}
		return g
	case []any:
		g, ok := got.([]any)
		if !ok {
			return got
		}
		for i := range g {
			if i < len(w) {
				g[i] = oseqNormalize(g[i], w[i], ids)
			}
		}
		return g
	case string:
		gs, ok := got.(string)
		if !ok {
			return got
		}
		if w == "2099-01-01T00:00:00.000001+00:00" && regexp.MustCompile(`^\d{4}-\d\d-\d\dT`).MatchString(gs) {
			return w
		}
		if oseqNormUUID.MatchString(w) && oseqRealUUID.MatchString(gs) {
			if prev, seen := ids[gs]; !seen || prev == w {
				ids[gs] = w
				return w
			}
		}
	}
	return got
}

func (e oseqEnv) replay(t *testing.T, order []string, pick func(string) bool) {
	t.Helper()
	real := map[string]string{} // recorded normalised id -> real id
	norm := map[string]string{} // real id -> recorded normalised id
	for _, name := range order {
		fx := contract.Load(t, "outreach/"+name)
		who, ws := oseqActor(name)
		path := oseqNormUUID.ReplaceAllStringFunc(fx.Request.Path, func(s string) string {
			if r, ok := real[s]; ok {
				return r
			}
			return s
		})
		body := ""
		if b := strings.TrimSpace(string(fx.Request.Body)); b != "" && b != "null" {
			body = b
		}
		rec := e.call(who, ws, fx.Request.Method, path, body)
		var gotBody, wantBody any
		_ = json.Unmarshal(rec.Body.Bytes(), &gotBody)
		_ = json.Unmarshal(fx.Response.Body, &wantBody)
		if m, ok := gotBody.(map[string]any); ok && rec.Code == 200 && fx.Request.Method == "POST" {
			if w, _ := wantBody.(map[string]any)["id"].(string); w != "" {
				g, _ := m["id"].(string)
				real[w] = g
			}
		}
		if !pick(name) {
			continue
		}
		t.Run(name, func(t *testing.T) {
			if want, ok := oseqDeviations[name]; ok {
				if rec.Code != want || !strings.Contains(rec.Body.String(), "/start") {
					t.Fatalf("deviation %s: %d %s", name, rec.Code, rec.Body.String())
				}
				return
			}
			if name == "seq_get_after_put" { // the recorded run activated the sequence through PUT; Go refused (D3)
				wantBody.(map[string]any)["status"] = "paused"
			}
			gotBody = oseqNormalize(gotBody, wantBody, norm)
			gb, _ := json.Marshal(gotBody)
			wb, _ := json.Marshal(wantBody)
			got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: gb}
			for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
				if v := rec.Header().Get(k); v != "" {
					got.Headers[k] = v
				}
			}
			want := fx.Response
			want.Body = wb
			contract.AssertEqual(t, got, want)
		})
	}
}

func TestSequenceRoutesMatchFixtures(t *testing.T) {
	e := newOseqEnv(t)
	n := 0
	e.replay(t, oseqOrder, func(string) bool { n++; return true })
	if n != len(oseqOrder) || n < 30 {
		t.Fatalf("replayed %d of %d", n, len(oseqOrder))
	}
}

func (e oseqEnv) as(who, ws, method, path, body string) (int, map[string]any, string) {
	rec := e.call(who, ws, method, path, body)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m, rec.Body.String()
}

func (e oseqEnv) rowCount(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := wcrudOwnerQueryRow(t, q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateRequiresAdmin(t *testing.T) {
	e := newOseqEnv(t)
	for _, c := range []struct {
		who, ws string
		code    int
	}{{"erin", "", 403}, {"bob", "ws-alpha", 403}, {"", "", 401}, {"alice", "", 200}} {
		code, _, raw := e.as(c.who, c.ws, "POST", "/api/outreach/sequences", `{"name":"gate"}`)
		if code != c.code {
			t.Errorf("%s: %d %s want %d", c.who, code, raw, c.code)
		}
	}
	if n := e.rowCount(t, `SELECT count(*) FROM outreach_sequences WHERE name = 'gate'`); n != 1 {
		t.Errorf("rows named gate = %d, want 1 (only the admin created)", n)
	}
	// write routes check the role before validating the body
	if code, _, _ := e.as("erin", "", "POST", "/api/outreach/sequences", `{"daily_limit":"x"}`); code != 403 {
		t.Errorf("editor with invalid body: %d want 403", code)
	}
	for _, m := range []string{"PUT", "DELETE"} {
		if code, _, _ := e.as("erin", "", m, "/api/outreach/sequences/seq-out-a-02", `{"name":"x"}`); code != 403 {
			t.Errorf("editor %s: %d want 403", m, code)
		}
	}
}

func TestViewerCanList(t *testing.T) {
	e := newOseqEnv(t)
	for _, who := range []string{"bob", "erin"} {
		ws := ""
		if who == "bob" {
			ws = "ws-alpha"
		}
		code, m, raw := e.as(who, ws, "GET", "/api/outreach/sequences", "")
		seqs, _ := m["sequences"].([]any)
		if code != 200 || len(seqs) != 2 {
			t.Errorf("%s list: %d %s", who, code, raw)
		}
		if code, _, raw := e.as(who, ws, "GET", "/api/outreach/sequences/seq-out-a-01", ""); code != 200 || !strings.Contains(raw, `"stats"`) {
			t.Errorf("%s get: %d %s", who, code, raw)
		}
	}
}

func TestUpdateOnlyGivenFieldsPersisted(t *testing.T) {
	e := newOseqEnv(t)
	_, m, _ := e.as("alice", "", "POST", "/api/outreach/sequences", `{"name":"keep","description":"d","daily_limit":7,"send_window_tz":"Asia/Kolkata","consent_basis":"consent"}`)
	id, _ := m["id"].(string)
	if code, m, raw := e.as("alice", "", "PUT", "/api/outreach/sequences/"+id, `{"description":"new","daily_limit":null}`); code != 200 || m["status"] != "ok" || m["id"] != id {
		t.Fatalf("put: %d %s", code, raw)
	}
	_, got, _ := e.as("alice", "", "GET", "/api/outreach/sequences/"+id, "")
	if got["name"] != "keep" || got["description"] != "new" || got["daily_limit"] != float64(7) || got["send_window_tz"] != "Asia/Kolkata" ||
		got["consent_basis"] != "consent" || got["status"] != "draft" {
		t.Fatalf("untouched fields changed: %v", got)
	}
}

func TestUpdateUnknown404(t *testing.T) {
	e := newOseqEnv(t)
	code, m, _ := e.as("alice", "", "PUT", "/api/outreach/sequences/nope", `{"name":"x"}`)
	if code != 404 || m["detail"] != "Sequence not found" {
		t.Fatalf("%d %v", code, m)
	}
	// an empty body on an unknown id is still 404
	if code, _, _ := e.as("alice", "", "PUT", "/api/outreach/sequences/nope", `{}`); code != 404 {
		t.Fatalf("empty body: %d", code)
	}
}

func TestDeleteUnknown404(t *testing.T) {
	e := newOseqEnv(t)
	code, m, _ := e.as("alice", "", "DELETE", "/api/outreach/sequences/nope", "")
	if code != 404 || m["detail"] != "Sequence not found" {
		t.Fatalf("%d %v", code, m)
	}
}

func TestDeleteRemovesDependentRows(t *testing.T) {
	e := newOseqEnv(t)
	if code, m, raw := e.as("alice", "", "DELETE", "/api/outreach/sequences/seq-out-a-01", ""); code != 200 || m["status"] != "ok" || len(m) != 1 {
		t.Fatalf("%d %s", code, raw)
	}
	for _, q := range []string{`SELECT count(*) FROM outreach_sends WHERE sequence_id='seq-out-a-01'`,
		`SELECT count(*) FROM outreach_enrollments WHERE sequence_id='seq-out-a-01'`,
		`SELECT count(*) FROM outreach_sequences WHERE id='seq-out-a-01'`} {
		if n := e.rowCount(t, q); n != 0 {
			t.Errorf("%s = %d", q, n)
		}
	}
}

func TestForeignWorkspace404(t *testing.T) {
	e := newOseqEnv(t)
	for _, c := range []struct{ method, body string }{{"GET", ""}, {"PUT", `{"name":"hijack"}`}, {"DELETE", ""}} {
		code, m, raw := e.as("alice", "", c.method, "/api/outreach/sequences/seq-out-b-01", c.body)
		if code != 404 || m["detail"] != "Sequence not found" {
			t.Errorf("%s foreign: %d %s", c.method, code, raw)
		}
	}
	if n := e.rowCount(t, `SELECT count(*) FROM outreach_sequences WHERE id='seq-out-b-01' AND name='Beta sequence'`); n != 1 {
		t.Error("foreign sequence was modified or deleted")
	}
	// list never leaks the other workspace
	_, _, raw := e.as("alice", "", "GET", "/api/outreach/sequences", "")
	if strings.Contains(raw, "seq-out-b-01") {
		t.Errorf("list leaked ws-beta: %s", raw)
	}
}

func TestCreateOverlongFieldsAre422Over_HTTP(t *testing.T) {
	e := newOseqEnv(t)
	for _, body := range []string{
		`{"name":"` + strings.Repeat("n", 201) + `"}`,
		`{"name":"n","send_window_tz":"` + strings.Repeat("z", 41) + `"}`,
		`{"name":"n","consent_basis":"` + strings.Repeat("c", 41) + `"}`,
		`{"name":"n","daily_limit":2147483648}`,
		`{"name":"n","send_window_tz":"Mars/Olympus"}`,
		`{"name":"a\u0000b"}`,
	} {
		code, m, raw := e.as("alice", "", "POST", "/api/outreach/sequences", body)
		if _, isList := m["detail"].([]any); code != 422 || !isList {
			t.Errorf("%.60s: %d %s", body, code, raw)
		}
	}
	if n := e.rowCount(t, `SELECT count(*) FROM outreach_sequences WHERE workspace_id='ws-alpha'`); n != 2 {
		t.Errorf("a refused create left a row: %d", n)
	}
}

func TestPutActiveStatusRefusedOverHTTP(t *testing.T) {
	e := newOseqEnv(t)
	code, m, raw := e.as("alice", "", "PUT", "/api/outreach/sequences/seq-out-a-02", `{"status":"active"}`)
	if _, isList := m["detail"].([]any); code != 422 || !isList || !strings.Contains(raw, "start") {
		t.Fatalf("%d %s", code, raw)
	}
	if code, _, _ := e.as("alice", "", "PUT", "/api/outreach/sequences/nope", `{"status":"active"}`); code != 422 {
		t.Errorf("validation precedes the lookup: %d", code)
	}
	if n := e.rowCount(t, `SELECT count(*) FROM outreach_sequences WHERE id='seq-out-a-02' AND status='draft'`); n != 1 {
		t.Error("status changed")
	}
	if code, _, raw := e.as("alice", "", "PUT", "/api/outreach/sequences/seq-out-a-02", `{"status":"paused"}`); code != 200 {
		t.Errorf("paused: %d %s", code, raw)
	}
}

func TestSequencesTenantTxScopesWorkspace(t *testing.T) {
	e := newOseqEnv(t)
	code, m, _ := e.as("bob", "ws-beta", "GET", "/api/outreach/sequences/seq-out-a-01", "")
	if code != 404 || m["detail"] != "Sequence not found" {
		t.Fatalf("bob read alpha sequence: %d %v", code, m)
	}
}
