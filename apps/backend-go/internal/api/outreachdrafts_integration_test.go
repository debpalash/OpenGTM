//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

var odrFixtures = strings.Fields(`drafts_list_default drafts_list_limit1_offset1 drafts_list_offset_past_end drafts_list_limit200
drafts_list_limit0_422 drafts_list_limit201_422 drafts_list_offset_negative_422 drafts_list_limit_text_422 drafts_list_beta_bob
drafts_list_viewer_bob_alpha drafts_list_nonmember drafts_list_no_auth draft_get_with_evidence draft_get_sent_risky
draft_get_foreign_404 draft_get_unknown_404 draft_get_no_auth`)

func odrActor(name string) (who, ws string) {
	switch name {
	case "drafts_list_beta_bob":
		return "bob", "ws-beta"
	case "drafts_list_viewer_bob_alpha":
		return "bob", "ws-alpha"
	case "drafts_list_nonmember":
		return "erin", "ws-beta"
	}
	if strings.HasSuffix(name, "no_auth") {
		return "", ""
	}
	return "alice", ""
}

func odrPurge() {
	wcrudOwnerExecQuiet(`DELETE FROM outreach_drafts WHERE workspace_id IN ('ws-alpha','ws-beta')`)
}

type odrRow struct {
	id, ws, key, conv, act, pid, pname, co, title, to, cs string
	risky, gen, role                                      bool
	subj, body, ev, snap, state                           string
	sent                                                  *string
	ts                                                    string
}

var odrSeed = []odrRow{
	{"d-out-1", "ws-alpha", "idem-draft-1", "conv-1", "act-1", "person-1", "Sam Lee", "Hot Pursuit Labs", "CTO", "ceo@hotpursuit.example.com", "verified",
		false, false, false, "Quick note on analytics", "Hi Sam,\n\nSaw the hiring push at Hot Pursuit Labs.\n\nBest",
		`[{"sentence": "Saw the hiring push at Hot Pursuit Labs.", "source_id": "sig-a-01", "url": "https://signals.example.com/a/1"}]`,
		`{"company": "Hot Pursuit Labs", "signals": ["hiring"]}`, "draft", nil, "2026-08-29 08:00:00+00"},
	{"d-out-2", "ws-alpha", "idem-draft-2", "conv-1", "act-2", "person-2", "Rhea Kapoor", "Zenith Robotics Pvt Ltd", "", "info@zenithrobotics.example.com", "risky",
		true, true, true, "Warehouse robots", "Hi Rhea,\n\nCongrats on the funding.",
		`[{"sentence": "Congrats on the funding.", "source_id": "sig-a-02", "url": "https://signals.example.com/a/2"}]`,
		`{"company": "Zenith Robotics Pvt Ltd"}`, "sent", strp("2026-08-29 09:00:00+00"), "2026-08-29 09:00:00+00"},
	{"d-out-b-1", "ws-beta", "idem-draft-b1", "conv-b1", "act-b1", "person-b1", "Beta Person", "Initech Two", "", "p@initech2.example.com", "verified",
		false, false, false, "Beta draft", "Hi Beta.", `[]`, `{}`, "draft", nil, "2026-08-29 08:00:00+00"},
}

func strp(s string) *string { return &s }

func insertDraft(t *testing.T, r odrRow) {
	t.Helper()
	ownerExec(t, `INSERT INTO outreach_drafts (id, workspace_id, action_idempotency_key, conversation_id, source_action_id, person_id, person_name,
		company, title, to_email, contact_status, risky_approved, generic_inbox, is_role_address, subject, body_text, sentence_evidence,
		source_snapshot, state, sent_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,CAST($17 AS json),CAST($18 AS json),$19,$20,$21,$21)`,
		r.id, r.ws, r.key, r.conv, r.act, r.pid, r.pname, r.co, r.title, r.to, r.cs, r.risky, r.gen, r.role, r.subj, r.body, r.ev, r.snap,
		r.state, r.sent, r.ts)
}

func newOdrEnv(t *testing.T) oseqEnv {
	t.Helper()
	e := newLeadsEnv(t)
	e.seed(t, "erin", pythonHash, true, "user", false)
	e.member(t, "ws-alpha", e.mustUserByName(t, "erin").ID, "editor")
	odrPurge()
	t.Cleanup(odrPurge)
	for _, r := range odrSeed {
		insertDraft(t, r)
	}
	d := Deps{Pool: e.pool, Signer: testSigner, Identity: e.st, Flags: config.Flags{PGLeadStore: true, Outreach: config.OutreachSettings{GoRoutesEnabled: true}}}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(e.pool, nil))
	registerOutreachDrafts(r, d)
	e.h = r.Handler()
	return oseqEnv{e}
}

func (e oseqEnv) replayDrafts(t *testing.T, names []string) {
	t.Helper()
	for _, name := range names {
		fx := contract.Load(t, "outreach/"+name)
		who, ws := odrActor(name)
		t.Run(name, func(t *testing.T) {
			rec := e.call(who, ws, fx.Request.Method, fx.Request.Path, "")
			got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: rec.Body.Bytes()}
			for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
				if v := rec.Header().Get(k); v != "" {
					got.Headers[k] = v
				}
			}
			contract.AssertEqual(t, got, fx.Response)
		})
	}
}

func TestDraftsListMatchesFixtures(t *testing.T) {
	e := newOdrEnv(t)
	e.replayDrafts(t, odrFixtures[:9])
}

func TestDraftsPaginationBounds422(t *testing.T) {
	e := newOdrEnv(t)
	e.replayDrafts(t, []string{"drafts_list_limit0_422", "drafts_list_limit201_422", "drafts_list_offset_negative_422", "drafts_list_limit_text_422"})
	for _, q := range []string{"limit=1.5", "limit=", "offset=abc", "limit=99999999999999999999999"} {
		if code, _, body := e.as("alice", "", "GET", "/api/outreach/drafts?"+q, ""); code != 422 {
			t.Fatalf("%s: %d %s", q, code, body)
		}
	}
	// pydantic lax ints: whitespace, underscores and a zero fraction are accepted
	if code, m, body := e.as("alice", "", "GET", "/api/outreach/drafts?limit=%201_0&offset=0.0", ""); code != 200 || m["limit"] != float64(10) {
		t.Fatalf("lax int: %d %s", code, body)
	}
}

func TestDraftGetWithEvidence(t *testing.T) {
	e := newOdrEnv(t)
	e.replayDrafts(t, []string{"draft_get_with_evidence", "draft_get_sent_risky", "draft_get_unknown_404", "draft_get_no_auth"})
	code, m, _ := e.as("alice", "", "GET", "/api/outreach/drafts/d-out-1", "")
	if code != 200 || m["sentence_evidence"] == nil || m["source_snapshot"] == nil {
		t.Fatalf("get: %d %v", code, m)
	}
	_, l, _ := e.as("alice", "", "GET", "/api/outreach/drafts", "")
	for _, d := range l["drafts"].([]any) {
		dm := d.(map[string]any)
		if _, has := dm["sentence_evidence"]; has {
			t.Fatal("list leaked sentence_evidence")
		}
		if _, has := dm["source_snapshot"]; has {
			t.Fatal("list leaked source_snapshot")
		}
	}
}

func TestDraftForeignWorkspace404(t *testing.T) {
	e := newOdrEnv(t)
	e.replayDrafts(t, []string{"draft_get_foreign_404"})
	if code, _, _ := e.as("bob", "ws-beta", "GET", "/api/outreach/drafts/d-out-1", ""); code != 404 {
		t.Fatalf("alpha draft via beta: %d", code)
	}
	if code, _, _ := e.as("bob", "ws-beta", "GET", "/api/outreach/drafts/d-out-b-1", ""); code != 200 {
		t.Fatalf("own beta draft: %d", code)
	}
}

// TestDraftKeyOrderMatchesPython: fixtures are sort_keys, so the order comes from _draft_to_api; evidence rows keep the
// stored key order, and HTML in a body is not escaped.
func TestDraftKeyOrderMatchesPython(t *testing.T) {
	e := newOdrEnv(t)
	insertDraft(t, odrRow{"d-out-html", "ws-alpha", "idem-html", "c", "a", "p9", "Zoë <Z>", "A & B", "", "z@x.example.com", "verified",
		false, false, false, "S", "<p>Hi & bye</p> é",
		`[{"url": "https://x.example.com/?a=1&b=2", "sentence": "café <b>", "source_id": "s1"}]`, `{"z": 1, "a": {"y": 2.50, "b": [1, 2], "f": 1e2}}`,
		"draft", nil, "2026-08-29 10:00:00.5+00"})
	rec := e.call("alice", "", "GET", "/api/outreach/drafts/d-out-html", "")
	want := `{"id":"d-out-html","person_id":"p9","person_name":"Zoë <Z>","company":"A & B","title":"","to_email":"z@x.example.com",` +
		`"contact_status":"verified","risky_approved":false,"generic_inbox":false,"is_role_address":false,"subject":"S",` +
		`"body_text":"<p>Hi & bye</p> é","state":"draft","sent_at":null,"send_performed":false,` +
		`"created_at":"2026-08-29T10:00:00.500000+00:00","updated_at":"2026-08-29T10:00:00.500000+00:00",` +
		`"sentence_evidence":[{"url":"https://x.example.com/?a=1&b=2","sentence":"café <b>","source_id":"s1"}],` +
		`"source_snapshot":{"z":1,"a":{"y":2.5,"b":[1,2],"f":100.0}}}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("key order / escaping\n got %s\nwant %s", got, want)
	}
	rec = e.call("alice", "", "GET", "/api/outreach/drafts?limit=1", "")
	var buf bytes.Buffer
	_ = json.Compact(&buf, rec.Body.Bytes())
	if !strings.HasPrefix(buf.String(), `{"drafts":[{"id":"d-out-html",`) || !strings.HasSuffix(buf.String(), `"limit":1,"offset":0}`) {
		t.Fatalf("list envelope: %s", buf.String())
	}
}

func TestDraftsViewerAllowed(t *testing.T) {
	e := newOdrEnv(t)
	e.replayDrafts(t, []string{"drafts_list_viewer_bob_alpha", "drafts_list_nonmember", "drafts_list_no_auth"})
	if code, _, _ := e.as("bob", "ws-alpha", "GET", "/api/outreach/drafts/d-out-1", ""); code != 200 {
		t.Fatalf("viewer get: %d", code)
	}
	if code, _, _ := e.as("bob", "ws-alpha", "POST", "/api/outreach/drafts", "{}"); code != 405 && code != 404 {
		t.Fatalf("no write route expected, got %d", code)
	}
}
