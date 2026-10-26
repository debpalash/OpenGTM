//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// ckEnv serves the conversation, memory and info routes over the recording seed
// (contract_seed.py _seed_copilot): erin owns conv-ed-1..3 in ws-alpha, bob owns conv-b-1 in ws-beta.
type ckEnv struct {
	leadsEnv
	erin, frank int32
}

func ckPurge() {
	for _, q := range []string{
		`DELETE FROM chat_messages WHERE workspace_id IN ('ws-alpha','ws-beta')`,
		`DELETE FROM chat_conversations WHERE workspace_id IN ('ws-alpha','ws-beta')`,
		`DELETE FROM chat_memories WHERE user_id LIKE 'ws-alpha:%' OR user_id LIKE 'ws-beta:%'`,
	} {
		wcrudOwnerExecQuiet(q)
	}
}

func newCKEnv(t *testing.T, requireAuth bool) *ckEnv {
	t.Helper()
	le := newLeadsEnv(t)
	ckPurge()
	t.Cleanup(ckPurge)
	d := Deps{Pool: le.pool, Signer: testSigner, Identity: le.st,
		Flags: config.Flags{PGLeadStore: true, Copilot: config.CopilotSettings{GoCopilotEnabled: true, ChatRequireAuth: requireAuth}}}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(le.pool, nil))
	RegisterCopilot(r, d)
	le.h = r.Handler()
	e := &ckEnv{leadsEnv: le}
	le.seed(t, "erin", pythonHash, true, "user", false)
	le.seed(t, "frank", pythonHash, true, "user", false)
	le.seed(t, "ivy", pythonHash, false, "user", false)
	e.erin, e.frank = le.mustUserByName(t, "erin").ID, le.mustUserByName(t, "frank").ID
	le.member(t, "ws-alpha", e.erin, "editor")
	le.member(t, "ws-alpha", le.mustUserByName(t, "ivy").ID, "member")
	if requireAuth {
		e.seedChat(t)
	}
	return e
}

// ckFixture loads the raw fixture JSON of one recorded case.
func ckFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "contract", "copilot", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// seedChat inserts the recorded conversations and messages (read back from the recorded GET fixtures, so the
// stored text, tool_data strings and timestamps are exactly what Python stored) and the recorded memories.
func (e *ckEnv) seedChat(t *testing.T) {
	t.Helper()
	owners := map[string]struct {
		ws  string
		uid int32
	}{"conv-ed-1": {"ws-alpha", e.erin}, "conv-ed-2": {"ws-alpha", e.erin}, "conv-ed-3": {"ws-alpha", e.erin},
		"conv-b-1": {"ws-beta", e.mustUserByName(t, "bob").ID}}
	for _, fx := range []string{"conv_get_owner_people", "conv_get_owner_contacts", "conv_get_owner_stats", "conv_get_beta_owner"} {
		body := ckFixture(t, fx)["response"].(map[string]any)["body"].(map[string]any)
		c := body["conversation"].(map[string]any)
		o := owners[c["id"].(string)]
		ownerExec(t, `INSERT INTO chat_conversations (id, title, workspace_id, user_id, created_at, updated_at) VALUES ($1,$2,$3,$4,$5::timestamptz,$6::timestamptz)`,
			c["id"], c["title"], o.ws, itoa32(o.uid), c["created_at"].(string)+"+00", c["updated_at"].(string)+"+00")
		for _, mm := range body["messages"].([]any) {
			m := mm.(map[string]any)
			ownerExec(t, `INSERT INTO chat_messages (id, conversation_id, workspace_id, role, content, tool_data, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7::timestamptz)`,
				m["id"], m["conversation_id"], o.ws, m["role"], m["content"], m["tool_data"], m["created_at"].(string)+"+00")
		}
	}
	for _, m := range []struct{ ns, text, md, ts string }{
		{"ws-alpha:E", "User asked: find hiring partnership leads in Pune\nAssistant answered about: five leads", `{"conversation_id": "conv-ed-3", "type": "episodic"}`, "2026-08-20T09:00:00+00:00"},
		{"ws-alpha:E", "User prefers weekly signal tracking for partnership hiring", `{"type": "preference"}`, "2026-08-21T09:00:00+00:00"},
		{"ws-alpha:E", "User asked: show stripe partnership team\nAssistant answered about: Stripe people table", `{"conversation_id": "conv-ed-1", "type": "episodic"}`, "2026-08-22T09:00:00+00:00"},
		{"ws-alpha:E", "Pipeline review every Monday for Pune staffing accounts", `{}`, "2026-08-23T09:00:00+00:00"},
		{"ws-alpha:E", "Café partnership notes: ÉCOLE Berlin, STRIPE contact 42", `{"type": "note"}`, "2026-08-24T09:00:00+00:00"},
		{"ws-alpha:E", "the and for with", `{}`, "2026-08-25T09:00:00+00:00"},
		{"ws-beta:B", "Beta tenant prefers email outreach to Berlin agencies", `{"type": "preference"}`, "2026-08-20T09:00:00+00:00"},
		{"ws-beta:B", "User asked: show stripe partnership team", `{"type": "episodic"}`, "2026-08-21T09:00:00+00:00"},
	} {
		ns := strings.Replace(strings.Replace(m.ns, ":E", ":"+itoa32(e.erin), 1), ":B", ":"+itoa32(e.mustUserByName(t, "bob").ID), 1)
		ownerExec(t, `INSERT INTO chat_memories (user_id, text, metadata, created_at) VALUES ($1,$2,$3::jsonb,$4::timestamptz)`, ns, m.text, m.md, m.ts)
	}
}

func itoa32(n int32) string { b, _ := json.Marshal(n); return string(b) }

func (e *ckEnv) req(method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	rec, _ := e.do(method, path, "", "", hdr)
	return rec
}

func (e *ckEnv) as(who, ws string) map[string]string {
	h := map[string]string{}
	if who != "" {
		h = bearer(who)
	}
	if ws != "" {
		h["X-Workspace-Id"] = ws
	}
	return h
}

// ckWho says who sent a recorded request (the recorder redacts Authorization and X-Workspace-Id). The recorder's
// default workspace resolution is replaced by an explicit header: the seed has no active-workspace preference.
type ckWho struct {
	who, ws string
	hdr     map[string]string
}

func ckCases(t *testing.T, e *ckEnv) map[string]ckWho {
	refresh, err := testSigner.Refresh("alice", []string{"pwd"})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]ckWho{
		"info_get": {}, "info_post": {}, "info_no_auth_needed": {who: "alice"},
		"auth_get_no_header": {}, "auth_delete_no_header": {}, "auth_list_no_header": {}, "auth_memories_no_header": {},
		"auth_list_bad_scheme":        {hdr: map[string]string{"Authorization": "Basic abc"}},
		"auth_list_garbage_token":     {hdr: map[string]string{"Authorization": "Bearer garbage"}},
		"auth_list_refresh_token":     {hdr: map[string]string{"Authorization": "Bearer " + refresh}},
		"auth_list_inactive_user":     {who: "ivy", ws: "ws-alpha"},
		"auth_list_no_workspace":      {who: "frank"},
		"auth_list_non_member":        {who: "frank", ws: "ws-alpha"},
		"auth_list_unknown_workspace": {who: "alice", ws: "ws-nope"},
		"conv_list_alice_empty":       {who: "alice", ws: "ws-alpha"}, "conv_list_erin": {who: "erin", ws: "ws-alpha"},
		"conv_list_bob_beta": {who: "bob", ws: "ws-beta"}, "conv_list_bob_in_alpha_empty": {who: "bob", ws: "ws-alpha"},
		"conv_get_owner_people": {who: "erin", ws: "ws-alpha"}, "conv_get_owner_contacts": {who: "erin", ws: "ws-alpha"},
		"conv_get_owner_stats": {who: "erin", ws: "ws-alpha"}, "conv_get_beta_owner": {who: "bob", ws: "ws-beta"},
		"conv_get_beta_untouched": {who: "bob", ws: "ws-beta"}, "conv_get_other_user": {who: "bob", ws: "ws-alpha"},
		"conv_get_other_workspace": {who: "bob", ws: "ws-beta"}, "conv_get_owner_wrong_workspace_header": {who: "bob", ws: "ws-alpha"},
		"conv_get_unknown":     {who: "erin", ws: "ws-alpha"},
		"memories_alice_empty": {who: "alice", ws: "ws-alpha"}, "memories_bob_beta": {who: "bob", ws: "ws-beta"},
		"memories_erin": {who: "erin", ws: "ws-alpha"},
	}
}

// ckStateful are recorded cases that depend on an earlier mutation or on rows the recorder created at run time;
// they are replayed by their own tests below.
var ckStateful = map[string]bool{
	"conv_delete_other_user_noop": true, "conv_delete_other_workspace_noop": true, "conv_delete_owner": true,
	"conv_delete_unknown_noop": true, "conv_get_after_delete": true, "conv_get_after_foreign_delete": true,
	"conv_list_after_delete": true, "auth_list_workspace_not_found": true, "blank_slug_beta": true, "deactivate_erin": true,
	"user_create_frank": true,
}

func TestCopilotRoutesMatchFixtures(t *testing.T) {
	e := newCKEnv(t, true)
	cases := ckCases(t, e)
	seen := 0
	for _, full := range contractFixtureNames(t, "copilot") {
		name := strings.TrimPrefix(full, "copilot/")
		isRoute := name == "info_get" || name == "info_post" || name == "info_no_auth_needed" ||
			strings.HasPrefix(name, "conv_") || strings.HasPrefix(name, "memories_") ||
			(strings.HasPrefix(name, "auth_") && !strings.HasPrefix(name, "auth_chat_"))
		if !isRoute || ckStateful[name] {
			continue
		}
		c, ok := cases[name]
		if !ok {
			t.Errorf("recorded fixture %s has no replay case", name)
			continue
		}
		seen++
		t.Run(name, func(t *testing.T) {
			fx := contract.Load(t, full)
			hdr := c.hdr
			if hdr == nil {
				hdr = e.as(c.who, c.ws)
			}
			rec, got := e.do(fx.Request.Method, fx.Request.Path, "", "", hdr)
			contract.AssertEqual(t, got, fx.Response)
			if rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("content-type %q", rec.Header().Get("Content-Type"))
			}
		})
	}
	if seen < 28 {
		t.Fatalf("replayed only %d recorded cases", seen)
	}
}

// Stateful recorded cases: deletes and the blank-slug lookup.
func TestCopilotStatefulFixtures(t *testing.T) {
	check := func(t *testing.T, e *ckEnv, name, who, ws string) {
		t.Helper()
		fx := contract.Load(t, "copilot/"+name)
		_, got := e.do(fx.Request.Method, fx.Request.Path, "", "", e.as(who, ws))
		contract.AssertEqual(t, got, fx.Response)
	}
	t.Run("foreign deletes are no-ops", func(t *testing.T) {
		e := newCKEnv(t, true)
		check(t, e, "conv_delete_other_user_noop", "bob", "ws-alpha")
		check(t, e, "conv_delete_other_workspace_noop", "bob", "ws-beta")
		check(t, e, "conv_delete_unknown_noop", "erin", "ws-alpha")
		check(t, e, "conv_get_after_foreign_delete", "erin", "ws-alpha")
	})
	t.Run("owner delete", func(t *testing.T) {
		e := newCKEnv(t, true)
		check(t, e, "conv_delete_owner", "erin", "ws-alpha")
		check(t, e, "conv_get_after_delete", "erin", "ws-alpha")
		if n := ckCount(t, `SELECT count(*) FROM chat_messages WHERE conversation_id='conv-ed-3'`); n != 0 {
			t.Fatalf("%d messages survived the delete", n)
		}
		if n := ckCount(t, `SELECT count(*) FROM chat_messages WHERE workspace_id='ws-alpha'`); n == 0 {
			t.Fatal("the delete took other conversations' messages")
		}
	})
	t.Run("blank slug is 404 Workspace not found", func(t *testing.T) {
		e := newCKEnv(t, true)
		ownerExec(t, `UPDATE workspaces SET slug='' WHERE id='ws-beta'`)
		check(t, e, "auth_list_workspace_not_found", "bob", "ws-beta")
	})
}

func ckCount(t *testing.T, q string) int {
	t.Helper()
	c, err := pgx.Connect(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	var n int
	if err := c.QueryRow(context.Background(), q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInfoUnauthenticated(t *testing.T) {
	e := newCKEnv(t, true)
	want := `{"actions":[{"name":"search_leads","description":"Search leads in the database"},{"name":"get_lead_stats","description":"Get pipeline statistics"},{"name":"update_lead_status","description":"Update a lead's status"},{"name":"start_collection","description":"Start collecting new leads"},{"name":"find_people_at_company","description":"Research people at one named company"}]}`
	for _, m := range []string{"GET", "POST"} {
		for _, hdr := range []map[string]string{nil, {"Authorization": "Bearer garbage"}, e.as("alice", "ws-nope")} {
			rec := e.req(m, "/api/copilotkit/info", hdr)
			if rec.Code != 200 || rec.Body.String() != want {
				t.Fatalf("%s %v: %d %q", m, hdr, rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("content-type %q", rec.Header().Get("Content-Type"))
			}
		}
	}
}

// Python key order: conversation id,title,created_at,updated_at; message id,conversation_id,role,content,tool_data,created_at;
// memory memory,metadata,created_at (the fixtures are sort_keys, so the order is asserted on the raw bytes).
func TestCopilotBodyKeyOrderAndNoHTMLEscaping(t *testing.T) {
	e := newCKEnv(t, true)
	ownerExec(t, `UPDATE chat_conversations SET title='Q&A <b> "x" '||chr(8232) WHERE id='conv-ed-1'`)
	ownerExec(t, `UPDATE chat_messages SET content='a<b>&c'||chr(8232)||chr(8233)||chr(92)||'u2028' WHERE id='msg-ed1-1'`)
	ownerExec(t, `INSERT INTO chat_memories (user_id, text, metadata, created_at) VALUES ($1, 'x<y>&z', '{"a":1,"b":[1.5]}', '2026-09-01T00:00:00+00')`, "ws-alpha:"+itoa32(e.erin))

	rec := e.req("GET", "/api/copilotkit/conversations", e.as("erin", "ws-alpha"))
	body := rec.Body.String()
	if !strings.HasPrefix(body, `{"conversations":[{"id":"conv-ed-1","title":"Q&A <b> \"x\" `+" "+`","created_at":"2026-08-29 09:00:00","updated_at":"2026-08-29 09:05:00"}`) {
		t.Fatalf("list bytes: %q", body)
	}
	rec = e.req("GET", "/api/copilotkit/conversations/conv-ed-1", e.as("erin", "ws-alpha"))
	body = rec.Body.String()
	for _, frag := range []string{
		`{"conversation":{"id":"conv-ed-1",`,
		`"messages":[{"id":"msg-ed1-1","conversation_id":"conv-ed-1","role":"user","content":"a<b>&c` + "  " + `\\u2028","tool_data":null,"created_at":"2026-08-29 09:00:01"}`,
	} {
		if !strings.Contains(body, frag) {
			t.Fatalf("missing %q in %q", frag, body)
		}
	}
	if strings.Contains(body, "\\"+"u003c") || strings.Contains(body, "\\"+"u0026") || strings.HasSuffix(body, "\n") {
		t.Fatalf("HTML escaped or trailing newline: %q", body)
	}
	rec = e.req("GET", "/api/copilotkit/memories", e.as("erin", "ws-alpha"))
	body = rec.Body.String()
	if !strings.HasPrefix(body, `{"memories":[{"memory":"x<y>&z","metadata":{"a":1,"b":[1.5]},"created_at":"`) || !strings.HasSuffix(body, `],"available":true}`) {
		t.Fatalf("memories bytes: %q", body)
	}
}

func TestConversationsOwnerOnly(t *testing.T) {
	e := newCKEnv(t, true)
	ids := func(rec *httptest.ResponseRecorder) []string {
		var out struct{ Conversations []struct{ ID string } }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		var s []string
		for _, c := range out.Conversations {
			s = append(s, c.ID)
		}
		return s
	}
	if got := ids(e.req("GET", "/api/copilotkit/conversations", e.as("erin", "ws-alpha"))); strings.Join(got, ",") != "conv-ed-1,conv-ed-2,conv-ed-3" {
		t.Fatalf("erin: %v (newest first)", got)
	}
	if got := ids(e.req("GET", "/api/copilotkit/conversations", e.as("alice", "ws-alpha"))); len(got) != 0 {
		t.Fatalf("a workspace owner must not see a member's conversations: %v", got)
	}
	// a stored foreign-user row in the same workspace stays invisible
	ownerExec(t, `INSERT INTO chat_conversations (id, title, workspace_id, user_id) VALUES ('conv-x','x','ws-alpha','self-host')`)
	if got := ids(e.req("GET", "/api/copilotkit/conversations", e.as("erin", "ws-alpha"))); len(got) != 3 {
		t.Fatalf("self-host partition leaked: %v", got)
	}
	// the list is capped at Python's default limit of 50
	for i := 0; i < 55; i++ {
		ownerExec(t, `INSERT INTO chat_conversations (id, title, workspace_id, user_id, updated_at) VALUES ($1,'bulk','ws-alpha',$2, now())`, "bulk-"+itoa32(int32(i)), itoa32(e.erin))
	}
	if got := ids(e.req("GET", "/api/copilotkit/conversations", e.as("erin", "ws-alpha"))); len(got) != 50 {
		t.Fatalf("limit: %d", len(got))
	}
}

func TestGetForeignConversation404Body(t *testing.T) {
	e := newCKEnv(t, true)
	for _, c := range []struct{ who, ws, id string }{
		{"bob", "ws-alpha", "conv-ed-1"}, {"alice", "ws-alpha", "conv-ed-1"}, {"bob", "ws-beta", "conv-ed-1"},
		{"erin", "ws-alpha", "conv-b-1"}, {"erin", "ws-alpha", "nope"},
	} {
		rec := e.req("GET", "/api/copilotkit/conversations/"+c.id, e.as(c.who, c.ws))
		if rec.Code != 404 || rec.Body.String() != `{"error":"Not found"}` || rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%v: %d %q", c, rec.Code, rec.Body.String())
		}
	}
}

func TestDeleteForeignIsNoopOk(t *testing.T) {
	e := newCKEnv(t, true)
	before := ckCount(t, `SELECT count(*) FROM chat_conversations`) + 1000*ckCount(t, `SELECT count(*) FROM chat_messages`)
	for _, c := range []struct{ who, ws, id string }{
		{"bob", "ws-alpha", "conv-ed-1"}, {"alice", "ws-alpha", "conv-ed-1"}, {"bob", "ws-beta", "conv-ed-1"}, {"erin", "ws-alpha", "nope"},
		{"erin", "ws-alpha", "conv-b-1"},
	} {
		rec := e.req("DELETE", "/api/copilotkit/conversations/"+c.id, e.as(c.who, c.ws))
		if rec.Code != 200 || rec.Body.String() != `{"ok":true}` {
			t.Fatalf("%v: %d %q", c, rec.Code, rec.Body.String())
		}
	}
	if after := ckCount(t, `SELECT count(*) FROM chat_conversations`) + 1000*ckCount(t, `SELECT count(*) FROM chat_messages`); after != before {
		t.Fatal("a foreign delete removed rows")
	}
}

func TestMemoriesNamespaceIsolation(t *testing.T) {
	e := newCKEnv(t, true)
	texts := func(who, ws string) []string {
		rec := e.req("GET", "/api/copilotkit/memories", e.as(who, ws))
		var out struct {
			Memories  []struct{ Memory string }
			Available bool
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || !out.Available {
			t.Fatalf("%d %q", rec.Code, rec.Body.String())
		}
		var s []string
		for _, m := range out.Memories {
			s = append(s, m.Memory)
		}
		return s
	}
	if got := texts("erin", "ws-alpha"); len(got) != 6 || got[0] != "the and for with" {
		t.Fatalf("erin: %q", got)
	}
	if got := texts("bob", "ws-beta"); len(got) != 2 || !strings.HasPrefix(got[0], "User asked") {
		t.Fatalf("bob: %q", got)
	}
	// same user, other workspace; same workspace, other user
	if got := texts("bob", "ws-alpha"); len(got) != 0 {
		t.Fatalf("bob in alpha: %q", got)
	}
	if got := texts("alice", "ws-alpha"); len(got) != 0 {
		t.Fatalf("alice in alpha: %q", got)
	}
	// a namespace that merely starts with the same characters is a different partition
	ownerExec(t, `INSERT INTO chat_memories (user_id, text, created_at) VALUES ($1, 'leak', now())`, "ws-alpha:"+itoa32(e.erin)+"0")
	if got := texts("erin", "ws-alpha"); len(got) != 6 {
		t.Fatalf("prefix leak: %q", got)
	}
}

func TestAuthStatusMatrix(t *testing.T) {
	e := newCKEnv(t, true)
	refresh, _ := testSigner.Refresh("alice", []string{"pwd"})
	type want struct {
		code   int
		detail string
		www    string
	}
	unauth := want{401, "Could not validate credentials", "Bearer"}
	for _, c := range []struct {
		name string
		hdr  map[string]string
		want want
	}{
		{"no header", nil, unauth},
		{"bad scheme", map[string]string{"Authorization": "Basic abc"}, unauth},
		{"empty bearer", map[string]string{"Authorization": "Bearer "}, unauth},
		{"garbage", map[string]string{"Authorization": "Bearer garbage"}, unauth},
		{"refresh token", map[string]string{"Authorization": "Bearer " + refresh}, unauth},
		{"unknown user", bearer("nobody-t17"), unauth},
		{"inactive user", e.as("ivy", "ws-alpha"), unauth},
		{"no workspace", e.as("frank", ""), want{403, "No accessible workspace. Ask an admin to add you to one.", ""}},
		{"non member", e.as("frank", "ws-alpha"), want{403, "Workspace access denied", ""}},
		{"unknown workspace", e.as("alice", "ws-nope"), want{403, "Workspace access denied", ""}},
	} {
		for _, r := range [][2]string{{"GET", "/api/copilotkit/conversations"}, {"GET", "/api/copilotkit/conversations/conv-ed-1"},
			{"DELETE", "/api/copilotkit/conversations/conv-ed-1"}, {"GET", "/api/copilotkit/memories"}} {
			rec := e.req(r[0], r[1], c.hdr)
			var body map[string]string
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != c.want.code || body["detail"] != c.want.detail || len(body) != 1 || rec.Header().Get("WWW-Authenticate") != c.want.www {
				t.Fatalf("%s %s %s: %d %q www=%q", c.name, r[0], r[1], rec.Code, rec.Body.String(), rec.Header().Get("WWW-Authenticate"))
			}
		}
	}
	// a refused delete deletes nothing
	if ckCount(t, `SELECT count(*) FROM chat_conversations WHERE id='conv-ed-1'`) != 1 {
		t.Fatal("unauthenticated delete removed a row")
	}
	// other methods are refused by the router, not served
	for _, r := range [][2]string{{"POST", "/api/copilotkit/conversations"}, {"PUT", "/api/copilotkit/memories"}, {"DELETE", "/api/copilotkit/info"}} {
		if rec := e.req(r[0], r[1], e.as("erin", "ws-alpha")); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%v: %d", r, rec.Code)
		}
	}
}

func TestSelfHostRoutes(t *testing.T) {
	e := newCKEnv(t, false)
	ownerExec(t, `INSERT INTO app_settings (key, value) VALUES ('ACTIVE_WORKSPACE','ws-alpha') ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`)
	t.Cleanup(func() { wcrudOwnerExecQuiet(`DELETE FROM app_settings WHERE key='ACTIVE_WORKSPACE'`) })
	// seed the recorded rows for the erin-owned conversation (self-host reads the "self-host" partition only)
	e.seedChat(t)
	for name, hdr := range map[string]map[string]string{"selfhost_list_empty": nil, "selfhost_get_404": nil, "selfhost_memories_empty": nil,
		"selfhost_list_garbage_auth_ignored": {"Authorization": "Bearer garbage"}, "selfhost_info": nil} {
		fx := contract.Load(t, "copilot/"+name)
		_, got := e.do(fx.Request.Method, fx.Request.Path, "", "", hdr)
		contract.AssertEqual(t, got, fx.Response)
	}
	ownerExec(t, `INSERT INTO chat_conversations (id, title, workspace_id, user_id) VALUES ('conv-sh','mine','ws-alpha','self-host')`)
	ownerExec(t, `INSERT INTO chat_memories (user_id, text, created_at) VALUES ('ws-alpha:self-host','remember me', now())`)
	if rec := e.req("GET", "/api/copilotkit/conversations", nil); !bytes.Contains(rec.Body.Bytes(), []byte(`"id":"conv-sh"`)) || bytes.Contains(rec.Body.Bytes(), []byte("conv-ed")) {
		t.Fatalf("%q", rec.Body.String())
	}
	if rec := e.req("GET", "/api/copilotkit/memories", nil); !bytes.Contains(rec.Body.Bytes(), []byte(`"memory":"remember me"`)) || bytes.Contains(rec.Body.Bytes(), []byte("Pune")) {
		t.Fatalf("%q", rec.Body.String())
	}
	if rec := e.req("DELETE", "/api/copilotkit/conversations/conv-ed-1", nil); rec.Code != 200 || ckCount(t, `SELECT count(*) FROM chat_conversations WHERE id='conv-ed-1'`) != 1 {
		t.Fatal("self-host deleted a user's conversation")
	}
}
