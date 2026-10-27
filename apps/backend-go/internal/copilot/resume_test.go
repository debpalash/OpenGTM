//go:build integration

package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chathistory"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/copilotauth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

type resumeEnv struct {
	*nativeEnv
	py *fakeLLM
}

func resumeSetup(t *testing.T) *resumeEnv {
	t.Helper()
	e := &resumeEnv{nativeEnv: nativeSetup(t), py: newFakeLLM(t)}
	e.py.toolFn = func(toolReq) (int, any) { return 200, map[string]any{"result": `{"ok": true}`} }
	return e
}

func (e *resumeEnv) turn(uid int32) *Turn {
	d := Deps{Pool: e.pool, Rounds: e.py.round, Native: e.py.native, Settings: settingsStub{}, ToolTimeout: 5 * time.Second}
	return &Turn{Deps: d, WS: e.ws, Slug: "alpha", UID: i32(uid), ConvID: "c1",
		Auth: copilotauth.Resolved{WorkspaceID: e.ws, Slug: "alpha", UserID: i32(uid)}}
}

// propose stores a pending approval for a tool call the way the gate does.
func (e *resumeEnv) propose(t *testing.T, ws string, uid int32, name string, args string) string {
	t.Helper()
	var id string
	err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
		var err error
		id, err = chathistory.CreateApproval(context.Background(), tx, ws, i32(uid), map[string]any{
			"id": "call_orig", "type": "function", "function": map[string]any{"name": name, "arguments": args}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func resolve(t *testing.T, tn *Turn, msgs *[]map[string]any, ap []Approved) (string, error) {
	t.Helper()
	rec := newRec()
	w, err := ssestream.New(rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = tn.ResolveApproved(context.Background(), w, msgs, ap)
	return rec.String(), err
}

func approve(id string) Approved { return Approved{ID: id, Decision: "approve"} }

func frames(body string) []string {
	var out []string
	for _, p := range strings.Split(body, "\n\n") {
		if strings.HasPrefix(p, "data: ") {
			out = append(out, strings.TrimPrefix(p, "data: "))
		}
	}
	return out
}

const invalidReason = "Approval is invalid, expired, already consumed, or belongs to another workspace."

func TestApproveExecutesOnceUnderConcurrentResubmit(t *testing.T) {
	e := resumeSetup(t)
	id := e.propose(t, e.ws, 1, "create_people_workbook", `{"name": "x"}`)
	var wg sync.WaitGroup
	bodies := make([]string, 2)
	for i := range bodies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			msgs := []map[string]any{}
			b, err := resolve(t, e.turn(1), &msgs, []Approved{approve(id)})
			if err != nil {
				t.Error(err)
			}
			bodies[i] = b
		}()
	}
	wg.Wait()
	if n := e.py.toolCount(); n != 1 {
		t.Fatalf("executions = %d, want exactly 1", n)
	}
	exec, denied := 0, 0
	for _, b := range bodies {
		switch {
		case strings.Contains(b, `{"tool_call": {"name": "create_people_workbook"`):
			exec++
		case strings.Contains(b, `{"tool_denied": {"name": "invalid_approval"}}`):
			denied++
		}
	}
	if exec != 1 || denied != 1 {
		t.Fatalf("exec=%d denied=%d bodies=%q", exec, denied, bodies)
	}
}

func TestDenyNeverExecutes(t *testing.T) {
	e := resumeSetup(t)
	id := e.propose(t, e.ws, 1, "update_lead_status", fmt.Sprintf(`{"lead_id": %d, "status": "dead"}`, e.acme))
	msgs := []map[string]any{}
	body, err := resolve(t, e.turn(1), &msgs, []Approved{{ID: id, Decision: "deny"}})
	if err != nil {
		t.Fatal(err)
	}
	if body != frame(`{"tool_denied": {"name": "update_lead_status"}}`) {
		t.Fatalf("body %q", body)
	}
	if e.py.toolCount() != 0 {
		t.Fatal("python tool ran")
	}
	var st string
	if err := e.owner.QueryRow(context.Background(), `SELECT status FROM leads WHERE id=$1`, e.acme).Scan(&st); err != nil || st != "new" {
		t.Fatalf("lead status %q %v", st, err)
	}
	m := msgs[1]
	if m["role"] != "tool" || m["tool_call_id"] != id || m["name"] != "update_lead_status" ||
		m["content"] != `{"denied": true, "reason": "User declined this action."}` {
		t.Fatalf("tool msg %v", m)
	}
	// denial burned the approval: a later approve cannot run it
	body, _ = resolve(t, e.turn(1), &msgs, []Approved{approve(id)})
	if !strings.Contains(body, "invalid_approval") || e.py.toolCount() != 0 {
		t.Fatalf("approve after deny: %q", body)
	}
}

func TestExpiredApprovalDenied(t *testing.T) {
	e := resumeSetup(t)
	id := e.propose(t, e.ws, 1, "create_people_workbook", `{}`)
	if _, err := e.owner.Exec(context.Background(),
		`UPDATE chat_tool_approvals SET created_at_epoch = created_at_epoch - 3700 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	msgs := []map[string]any{}
	body, err := resolve(t, e.turn(1), &msgs, []Approved{approve(id)})
	if err != nil {
		t.Fatal(err)
	}
	if body != frame(`{"tool_denied": {"name": "invalid_approval"}}`) || e.py.toolCount() != 0 {
		t.Fatalf("body %q tools %d", body, e.py.toolCount())
	}
}

func TestWrongWorkspaceApprovalDenied(t *testing.T) {
	e := resumeSetup(t)
	id := e.propose(t, e.other, 1, "create_people_workbook", `{}`)
	msgs := []map[string]any{}
	body, err := resolve(t, e.turn(1), &msgs, []Approved{approve(id)})
	if err != nil {
		t.Fatal(err)
	}
	if body != frame(`{"tool_denied": {"name": "invalid_approval"}}`) || e.py.toolCount() != 0 {
		t.Fatalf("body %q", body)
	}
	// and a different user in the same workspace cannot use it either
	id2 := e.propose(t, e.ws, 1, "create_people_workbook", `{}`)
	body, _ = resolve(t, e.turn(2), &msgs, []Approved{approve(id2)})
	if !strings.Contains(body, "invalid_approval") || e.py.toolCount() != 0 {
		t.Fatalf("other user: %q", body)
	}
}

func TestUnknownToolApprovedStillDenied(t *testing.T) {
	e := resumeSetup(t)
	id := e.propose(t, e.ws, 1, "search_leads", `{}`) // safe tool: not an explicitly classified mutation
	id2 := e.propose(t, e.ws, 1, "totally_made_up", `{}`)
	msgs := []map[string]any{}
	body, err := resolve(t, e.turn(1), &msgs, []Approved{approve(id), approve(id2)})
	if err != nil {
		t.Fatal(err)
	}
	want := frame(`{"tool_denied": {"name": "search_leads"}}`) + frame(`{"tool_denied": {"name": "totally_made_up"}}`)
	if body != want || e.py.toolCount() != 0 {
		t.Fatalf("body %q", body)
	}
	if c := msgs[1]["content"]; c != `{"denied": true, "reason": "Tool is not an explicitly classified mutation."}` {
		t.Fatalf("content %v", c)
	}
}

func TestRoleReCheckedAtExecution(t *testing.T) {
	e := resumeSetup(t)
	// proposed while the user was an editor, demoted to viewer (uid 3 is a viewer) before approval
	nativeID := e.propose(t, e.ws, 3, "update_lead_status", fmt.Sprintf(`{"lead_id": %d, "status": "dead"}`, e.acme))
	pyID := e.propose(t, e.ws, 3, "create_people_workbook", `{}`)
	msgs := []map[string]any{}
	body, err := resolve(t, e.turn(3), &msgs, []Approved{approve(nativeID), approve(pyID)})
	if err != nil {
		t.Fatal(err)
	}
	if e.py.toolCount() != 0 {
		t.Fatal("viewer reached the python worker")
	}
	var st string
	if err := e.owner.QueryRow(context.Background(), `SELECT status FROM leads WHERE id=$1`, e.acme).Scan(&st); err != nil || st != "new" {
		t.Fatalf("viewer changed the lead: %q %v", st, err)
	}
	if n := strings.Count(body, `"result": {"error": "Insufficient workspace role"}`); n != 2 {
		t.Fatalf("want 2 role errors in %q", body)
	}
	// an editor may execute the same kind of call
	okID := e.propose(t, e.ws, 2, "update_lead_status", fmt.Sprintf(`{"lead_id": %d, "status": "dead"}`, e.acme))
	if _, err := resolve(t, e.turn(2), &msgs, []Approved{approve(okID)}); err != nil {
		t.Fatal(err)
	}
	if err := e.owner.QueryRow(context.Background(), `SELECT status FROM leads WHERE id=$1`, e.acme).Scan(&st); err != nil || st != "dead" {
		t.Fatalf("editor change not applied: %q %v", st, err)
	}
}

func TestFollowUpContentPerTool(t *testing.T) {
	cases := []struct {
		tool, result string
		want         func(m map[string]any) string
	}{
		{"create_people_workbook", `{"workbook_id": "w1", "name": "Q3 People", "total_rows": 12, "url": "/wb/w1"}`,
			func(map[string]any) string {
				return "Created **Q3 People** with **12 people**. [Open workbook](/wb/w1)"
			}},
		{"create_people_workbook", `{"workbook_id": "w1", "url": "/wb/w1"}`,
			func(map[string]any) string {
				return "Created **People workbook** with **0 people**. [Open workbook](/wb/w1)"
			}},
		{"enrich_people_contacts", `{"ok": true, "summary": {}}`, func(m map[string]any) string { return FormatPeopleContacts(m) }},
		{"create_source_workbook", `{"workbook_id": "w2", "name": "Src", "source_job_id": "j9", "url": "/wb/w2"}`,
			func(map[string]any) string { return "Created **Src**. The source job is j9. [Open workbook](/wb/w2)" }},
		{"create_source_workbook", `{"workbook_id": "w2", "url": "/wb/w2"}`,
			func(map[string]any) string {
				return "Created **Sourcing workbook**. The source job is not started. [Open workbook](/wb/w2)"
			}},
		{"track_account_signals", `{"ok": true, "state": "active", "schedule_id": 4, "url": "/t/4", "signal_types": ["funding"], "scope": {"account_count": 2}}`,
			func(m map[string]any) string { return FormatSignalTracking(m) }},
		{"draft_grounded_outreach", `{"ok": true, "person_name": "Ann", "company": "Acme", "url": "/d/1"}`,
			func(m map[string]any) string { return FormatGroundedDraft(m) }},
	}
	for i, c := range cases {
		t.Run(fmt.Sprintf("%d_%s", i, c.tool), func(t *testing.T) {
			e := resumeSetup(t)
			res := c.result
			e.py.toolFn = func(toolReq) (int, any) { return 200, map[string]any{"result": res} }
			id := e.propose(t, e.ws, 1, c.tool, `{"a": 1}`)
			msgs := []map[string]any{}
			body, err := resolve(t, e.turn(1), &msgs, []Approved{approve(id)})
			if err != nil {
				t.Fatal(err)
			}
			v, _ := canonjson.Decode([]byte(res))
			fs := frames(body)
			if len(fs) != 3 {
				t.Fatalf("%s: frames %q", c.tool, fs)
			}
			if fs[0] != `{"tool_call": {"name": "`+c.tool+`", "args": {"a": 1}}}` {
				t.Fatalf("%s: tool_call %s", c.tool, fs[0])
			}
			if !strings.HasPrefix(fs[1], `{"tool_result": {"name": "`+c.tool+`", "result": {`) {
				t.Fatalf("%s: tool_result %s", c.tool, fs[1])
			}
			var wantFrame strings.Builder
			wantFrame.WriteString(`{"content": `)
			enc := json.NewEncoder(&wantFrame)
			enc.SetEscapeHTML(false)
			_ = enc.Encode(c.want(v.(map[string]any)))
			got := fs[2]
			exp := strings.TrimSuffix(wantFrame.String(), "\n") + "}"
			// Python's json.dumps escapes non-ASCII; the formatters emit an em dash in places, so compare decoded text
			var gotObj, expObj map[string]string
			if json.Unmarshal([]byte(got), &gotObj) != nil || json.Unmarshal([]byte(exp), &expObj) != nil || gotObj["content"] != expObj["content"] {
				t.Fatalf("%s: content frame %s\nwant %s", c.tool, got, exp)
			}
			if msgs[1]["content"] != res {
				t.Fatalf("%s: tool message content %v", c.tool, msgs[1]["content"])
			}
		})
	}
}

func TestNoFollowUpWithoutSuccess(t *testing.T) {
	e := resumeSetup(t)
	e.py.toolFn = func(toolReq) (int, any) { return 200, map[string]any{"result": `{"error": "nope"}`} }
	id := e.propose(t, e.ws, 1, "create_people_workbook", `{}`)
	body, _ := resolve(t, e.turn(1), &[]map[string]any{}, []Approved{approve(id)})
	if strings.Contains(body, `"content"`) {
		t.Fatalf("unexpected content: %q", body)
	}
}

func TestDuplicateIdInOneRequest(t *testing.T) {
	e := resumeSetup(t)
	id := e.propose(t, e.ws, 1, "create_people_workbook", `{}`)
	msgs := []map[string]any{}
	body, err := resolve(t, e.turn(1), &msgs, []Approved{approve(id), approve(id)})
	if err != nil {
		t.Fatal(err)
	}
	if e.py.toolCount() != 1 {
		t.Fatalf("executions %d", e.py.toolCount())
	}
	fs := frames(body)
	if len(fs) != 3 || fs[2] != `{"tool_denied": {"name": "invalid_approval"}}` {
		t.Fatalf("frames %q", fs)
	}
	// the assistant message carries the stored call, then the synthetic invalid one at index 1
	calls := msgs[0]["tool_calls"].([]any)
	if len(calls) != 2 || calls[1].(map[string]any)["id"] != id {
		t.Fatalf("calls %v", calls)
	}
	if msgs[2]["tool_call_id"] != id || msgs[2]["name"] != "invalid_approval" {
		t.Fatalf("invalid tool msg %v", msgs[2])
	}
}

func TestMessageOrderMatchesLegacy(t *testing.T) {
	e := resumeSetup(t)
	e.py.toolFn = func(toolReq) (int, any) { return 200, map[string]any{"result": `{"workbook_id": "w", "url": "/u"}`} }
	a := e.propose(t, e.ws, 1, "create_people_workbook", `{"k": "é"}`)
	b := e.propose(t, e.ws, 1, "enrich_people_contacts", ``)
	msgs := []map[string]any{{"role": "user", "content": "hi"}}
	body, err := resolve(t, e.turn(1), &msgs, []Approved{approve(a), {ID: "", Decision: "approve"}, {ID: b, Decision: "deny"}, {ID: "nope", Decision: "approve"}})
	if err != nil {
		t.Fatal(err)
	}
	wantFrames := []string{
		`{"tool_call": {"name": "create_people_workbook", "args": {"k": "\u00e9"}}}`,
		`{"tool_result": {"name": "create_people_workbook", "result": {"workbook_id": "w", "url": "/u"}}}`,
		`{"content": "Created **People workbook** with **0 people**. [Open workbook](/u)"}`,
		`{"tool_denied": {"name": "invalid_approval"}}`,
		`{"tool_denied": {"name": "enrich_people_contacts"}}`,
		`{"tool_denied": {"name": "invalid_approval"}}`,
	}
	if got := frames(body); strings.Join(got, "\n") != strings.Join(wantFrames, "\n") {
		t.Fatalf("frames\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantFrames, "\n"))
	}
	if len(msgs) != 6 || msgs[0]["role"] != "user" || msgs[1]["role"] != "assistant" || msgs[1]["content"] != "" {
		t.Fatalf("msgs %v", msgs)
	}
	calls := msgs[1]["tool_calls"].([]any)
	if len(calls) != 4 {
		t.Fatalf("calls %v", calls)
	}
	wantIDs := []any{a, "invalid-1", b, "nope"}
	for i, c := range calls {
		if c.(map[string]any)["id"] != wantIDs[i] {
			t.Fatalf("call %d id %v", i, c)
		}
	}
	if fn := calls[1].(map[string]any)["function"].(map[string]any); fn["name"] != "invalid_approval" || fn["arguments"] != "{}" {
		t.Fatalf("synthetic call %v", calls[1])
	}
	wantTool := []struct{ id, name string }{{a, "create_people_workbook"}, {"invalid-approval", "invalid_approval"}, {b, "enrich_people_contacts"}, {"nope", "invalid_approval"}}
	for i, w := range wantTool {
		m := msgs[2+i]
		if m["role"] != "tool" || m["tool_call_id"] != w.id || m["name"] != w.name {
			t.Fatalf("msg %d %v", 2+i, m)
		}
	}
	if msgs[2]["content"] != `{"workbook_id": "w", "url": "/u"}` {
		t.Fatalf("content %v", msgs[2]["content"])
	}
	if msgs[3]["content"] != `{"denied": true, "reason": "`+invalidReason+`"}` {
		t.Fatalf("invalid content %v", msgs[3]["content"])
	}
	// the executed call went out with the tool user and the stored (not client-echoed) arguments
	req := e.py.tools[0].Body
	if req["user_id"] != float64(1) || req["workspace_id"] != e.ws || req["name"] != "create_people_workbook" {
		t.Fatalf("tool body %v", req)
	}
}

func TestMoreThan20ItemsRejected(t *testing.T) {
	e := resumeSetup(t)
	id := e.propose(t, e.ws, 1, "create_people_workbook", `{}`)
	items := []Approved{approve(id)}
	for i := 0; i < 20; i++ {
		items = append(items, approve(fmt.Sprintf("x%d", i)))
	}
	msgs := []map[string]any{}
	body, err := resolve(t, e.turn(1), &msgs, items)
	if !errors.Is(err, ErrTooManyApprovals) {
		t.Fatalf("err %v", err)
	}
	if body != "" || len(msgs) != 0 || e.py.toolCount() != 0 {
		t.Fatalf("side effects: %q %v", body, msgs)
	}
	// nothing was consumed: exactly 20 items are accepted and run
	if _, err := resolve(t, e.turn(1), &msgs, items[:20]); err != nil || e.py.toolCount() != 1 {
		t.Fatalf("20 items: %v tools=%d", err, e.py.toolCount())
	}
}

func TestEmptyApprovedIsNoop(t *testing.T) {
	e := resumeSetup(t)
	msgs := []map[string]any{}
	body, err := resolve(t, e.turn(1), &msgs, nil)
	if err != nil || body != "" || len(msgs) != 0 {
		t.Fatalf("%q %v %v", body, msgs, err)
	}
}

func TestCancelDuringExecutionStaysConsumed(t *testing.T) {
	e := resumeSetup(t)
	started, release := make(chan struct{}, 1), make(chan struct{})
	e.py.toolFn = func(toolReq) (int, any) {
		started <- struct{}{}
		<-release
		return 200, map[string]any{"result": `{}`}
	}
	defer close(release)
	id := e.propose(t, e.ws, 1, "create_people_workbook", `{}`)
	ctx, cancel := context.WithCancel(context.Background())
	rec := newRec()
	w, _ := ssestream.New(rec, nil)
	errc := make(chan error, 1)
	go func() { errc <- e.turn(1).ResolveApproved(ctx, w, &[]map[string]any{}, []Approved{approve(id)}) }()
	<-started
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	body, _ := resolve(t, e.turn(1), &[]map[string]any{}, []Approved{approve(id)})
	if !strings.Contains(body, "invalid_approval") {
		t.Fatalf("approval re-usable after cancel: %q", body)
	}
}
