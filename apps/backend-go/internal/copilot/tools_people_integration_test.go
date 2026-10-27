//go:build integration

package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autopilot"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chathistory"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/entities"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadpeople"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Vectors: testdata/tools_{people,outreach,plan,preroute}.json, recorded by testdata/record_tools.py with
// recording fakes for people research / verification / contact enrichment (WP-PEOPLE owns the real ones).

type toolCase struct {
	Name    string          `json:"name"`
	Args    json.RawMessage `json:"args"`
	UserID  int             `json:"user_id"`
	History []struct {
		Name   string          `json:"name"`
		Result json.RawMessage `json:"result"`
	} `json:"history"`
	Result string           `json:"result"`
	Calls  []map[string]any `json:"calls"`
}

// fakePeople replays the recorder's fakes.
type fakePeople struct {
	research, verify map[string]any
	calls            []map[string]any
}

func plainCopy(v any) any {
	b, _ := json.Marshal(v)
	d, _ := canonjson.Decode(b)
	return d
}

func (f *fakePeople) Research(_ context.Context, company, function string, o leadpeople.ResearchOptions) (map[string]any, error) {
	f.calls = append(f.calls, map[string]any{"fn": "research", "company": company, "function": function,
		"titles": o.Titles, "location": o.Location, "limit": o.Limit})
	if company == "boom" {
		return nil, errors.New("search backend down")
	}
	return plainCopy(f.research).(map[string]any), nil
}

func (f *fakePeople) Verify(_ context.Context, company, function string, people []map[string]any) (map[string]any, error) {
	f.calls = append(f.calls, map[string]any{"fn": "verify", "company": company, "function": function, "people": plainCopy(people)})
	if company == "boom" {
		return nil, errors.New("verify backend down")
	}
	return plainCopy(f.verify).(map[string]any), nil
}

func (f *fakePeople) EnrichContacts(_ context.Context, company, function string, people []map[string]any, o leadpeople.ContactOptions) (map[string]any, error) {
	f.calls = append(f.calls, map[string]any{"fn": "enrich", "company": company, "function": function})
	ids := make([]any, len(o.PersonIDs))
	for i, s := range o.PersonIDs {
		ids[i] = s
	}
	return map[string]any{"ok": true, "company": company, "function": function, "action_id": o.ActionID,
		"selected_person_ids": ids, "people": []any{}, "count": int64(0)}, nil
}

func (f *fakePeople) ExecuteContactOnce(ctx context.Context, ws, actionID string, contract any, op func(context.Context) (map[string]any, error)) (map[string]any, error) {
	f.calls = append(f.calls, map[string]any{"fn": "once", "action_id": actionID, "contract": plainCopy(contract)})
	return op(ctx)
}

// sem decodes JSON for an order-insensitive comparison.
func sem(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("not JSON: %q", s)
	}
	return v
}

var uuidRx = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

type toolsEnv struct {
	*persistEnv
	convs map[int]string // user -> conversation
}

// Python user 7 is an editor (uid 2 here); Python user 99 lacks the role (viewer, uid 3).
func goUser(py int) int32 {
	if py == 99 {
		return 3
	}
	return 2
}

func toolsSetup(t *testing.T) *toolsEnv {
	e := &toolsEnv{persistEnv: persistSetup(t), convs: map[int]string{}}
	ctx := context.Background()
	for _, u := range []int32{2, 3} {
		if err := tenancy.InWorkspace(ctx, e.pool, e.ws, func(tx pgx.Tx) error {
			c, err := chathistory.Create(ctx, tx, e.ws, i32(u), "tools")
			e.convs[int(u)] = c.ID
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	clean := func() {
		for _, q := range []string{`DELETE FROM workbook_rows WHERE workspace_id = $1`, `DELETE FROM workbooks WHERE workspace_id = $1`,
			`DELETE FROM outreach_drafts WHERE workspace_id = $1`, `DELETE FROM chat_messages WHERE workspace_id = $1`} {
			_, _ = e.owner.Exec(ctx, q, e.ws)
		}
	}
	clean()
	t.Cleanup(clean)
	return e
}

// run seeds the conversation history, maps ids and runs the tool through the engine.
func (e *toolsEnv) run(t *testing.T, c toolCase, eng *Engine) (string, bool, error) {
	t.Helper()
	ctx := context.Background()
	uid := goUser(c.UserID)
	conv := e.convs[int(uid)]
	_, _ = e.owner.Exec(ctx, `DELETE FROM chat_messages WHERE workspace_id = $1`, e.ws)
	for _, h := range c.History {
		td := `{"name": ` + jstr(h.Name) + `, "result": ` + string(h.Result) + `}`
		if err := tenancy.InWorkspace(ctx, e.pool, e.ws, func(tx pgx.Tx) error {
			return chathistory.AddMessage(ctx, tx, conv, "tool", h.Name+" result", &td, e.ws, i32(uid))
		}); err != nil {
			t.Fatal(err)
		}
	}
	argsRaw := strings.ReplaceAll(string(c.Args), `"conv-1"`, jstr(conv))
	v, err := canonjson.Decode([]byte(argsRaw))
	if err != nil {
		t.Fatal(err)
	}
	tn := e.turn()
	tn.UID = i32(uid)
	tn.ConvID = conv
	tn.Engine = eng
	return tn.engineTool(ctx, parsedCall{Name: c.Name, Args: v.(map[string]any)})
}

func (e *toolsEnv) norm(s string) string {
	for _, c := range e.convs {
		s = strings.ReplaceAll(s, c, "conv-1")
	}
	return strings.ReplaceAll(uuidRx.ReplaceAllString(s, "$UUID"), e.ws, "ws-1")
}

func loadTools(t *testing.T, name string, v any) {
	t.Helper()
	readVectors(t, name, v)
}

func checkResult(t *testing.T, e *toolsEnv, c toolCase, got string, handled bool, err error) {
	t.Helper()
	if strings.Contains(c.Result, "backend down") { // Go reports an infrastructure error, not its text
		if err == nil {
			t.Fatalf("%s: want an error, got %s", c.Name, got)
		}
		return
	}
	if err != nil || !handled {
		t.Fatalf("%s %s: handled=%v err=%v", c.Name, c.Args, handled, err)
	}
	want := uuidRx.ReplaceAllString(c.Result, "$UUID")
	if g := e.norm(got); !reflect.DeepEqual(sem(t, g), sem(t, want)) {
		t.Fatalf("%s %s:\n got %s\nwant %s", c.Name, c.Args, g, want)
	}
}

func TestPeopleToolsMatchPython(t *testing.T) {
	var f struct {
		Research  map[string]any `json:"research_result"`
		Verify    map[string]any `json:"verify_result"`
		People    []toolCase     `json:"people"`
		Workbooks []toolCase     `json:"workbooks"`
		DB        struct {
			Workbooks []map[string]any `json:"workbooks"`
			Rows      []map[string]any `json:"rows"`
		} `json:"db"`
	}
	loadTools(t, "tools_people.json", &f)
	e := toolsSetup(t)
	fp := &fakePeople{research: f.Research, verify: f.Verify}
	var observed []PersonObservation
	eng := &Engine{People: fp, ResolvePerson: func(_ context.Context, _ pgx.Tx, ws string, o PersonObservation) (string, error) {
		observed = append(observed, o)
		return uuid.NewString(), nil
	}}
	for i, c := range f.People {
		fp.calls = nil
		got, handled, err := e.run(t, c, eng)
		checkResult(t, e, c, got, handled, err)
		// the people service received what Python's fakes received
		if len(fp.calls) != len(c.Calls) {
			t.Fatalf("%d %s: calls %v, want %v", i, c.Name, fp.calls, c.Calls)
		}
		for j, want := range c.Calls {
			gotCall := fp.calls[j]
			switch want["fn"] {
			case "research":
				kw := want["kwargs"].(map[string]any)
				limit := 8
				if l, ok := kw["limit"].(float64); ok && l != 0 {
					limit = int(l)
				}
				titles := []string{}
				for _, x := range kw["titles"].([]any) {
					titles = append(titles, x.(string))
				}
				if gotCall["company"] != kw["company"] || gotCall["function"] != kw["function"] || gotCall["location"] != kw["location"] ||
					gotCall["limit"] != limit || !reflect.DeepEqual(gotCall["titles"], titles) {
					t.Errorf("%d research call %v, want %v", i, gotCall, kw)
				}
			case "verify":
				kw := want["kwargs"].(map[string]any)
				gp, _ := json.Marshal(gotCall["people"])
				wp, _ := json.Marshal(kw["people"])
				if gotCall["company"] != kw["company"] || gotCall["function"] != kw["function"] || !reflect.DeepEqual(sem(t, string(gp)), sem(t, string(wp))) {
					t.Errorf("%d verify call %v, want %v", i, gotCall, kw)
				}
			case "once":
				gc, _ := json.Marshal(gotCall["contract"])
				wc, _ := json.Marshal(want["contract"])
				if e.norm(gotCall["action_id"].(string)) != want["action_id"] || !reflect.DeepEqual(sem(t, e.norm(string(gc))), sem(t, string(wc))) {
					t.Errorf("%d once call %s %s, want %v %s", i, gotCall["action_id"], gc, want["action_id"], wc)
				}
			}
		}
	}
	for _, c := range f.Workbooks {
		got, handled, err := e.run(t, c, eng)
		checkResult(t, e, c, got, handled, err)
	}
	// persisted workbooks and rows equal Python's (person ids are the fake resolver's)
	ctx := context.Background()
	q := func(sql string) []map[string]any {
		var s string
		if err := e.owner.QueryRow(ctx, `SELECT coalesce(json_agg(r), '[]')::text FROM (`+sql+`) r`, e.ws).Scan(&s); err != nil {
			t.Fatal(err)
		}
		var out []map[string]any
		_ = json.Unmarshal([]byte(e.norm(s)), &out)
		return out
	}
	gotWB := q(`SELECT name, description, status, source_type, action_idempotency_key, source_config, columns_config,
		total_rows, sync_to_leads FROM workbooks WHERE workspace_id = $1`)
	gotRows := q(`SELECT w.action_idempotency_key AS workbook_key, r.position, r.data, r.source_provider, r.source_record_id,
		r.corroboration_count FROM workbook_rows r JOIN workbooks w ON w.id = r.workbook_id WHERE r.workspace_id = $1`)
	canon := func(l []map[string]any) []string {
		var out []string
		for _, m := range l {
			b, _ := json.Marshal(semantic(m))
			out = append(out, uuidRx.ReplaceAllString(string(b), "$UUID"))
		}
		sort.Strings(out)
		return out
	}
	if g, w := canon(gotWB), canon(f.DB.Workbooks); !reflect.DeepEqual(g, w) {
		t.Fatalf("workbooks:\n got %v\nwant %v", g, w)
	}
	if g, w := canon(gotRows), canon(f.DB.Rows); !reflect.DeepEqual(g, w) {
		t.Fatalf("rows:\n got %v\nwant %v", g, w)
	}
	if len(observed) == 0 || observed[0].Source != "chat_people_research" || len(observed[0].LegacyIDs) != 1 {
		t.Fatalf("resolve_person observations %+v", observed)
	}
}

func TestPeopleToolsGoToWorkerWithoutLeadpeople(t *testing.T) {
	e := toolsSetup(t)
	tn := e.turn()
	tn.UID = i32(2)
	tn.Engine = &Engine{}
	for _, name := range []string{"find_people_at_company", "verify_people_at_company", "enrich_people_contacts", "create_people_workbook"} {
		if _, handled, err := tn.engineTool(context.Background(), parsedCall{Name: name, Args: map[string]any{}}); handled || err != nil {
			t.Fatalf("%s: handled=%v err=%v", name, handled, err)
		}
	}
}

func TestOutreachToolsMatchPython(t *testing.T) {
	var f struct {
		Leads   map[string]map[string]any `json:"leads"`
		Suggest []toolCase                `json:"suggest"`
		Draft   []toolCase                `json:"draft"`
	}
	loadTools(t, "tools_outreach.json", &f)
	e := toolsSetup(t)
	ctx := context.Background()
	base := int64(9300000)
	_, _ = e.owner.Exec(ctx, `DELETE FROM leads WHERE id BETWEEN $1 AND $2`, base, base+10)
	t.Cleanup(func() { _, _ = e.owner.Exec(ctx, `DELETE FROM leads WHERE id BETWEEN $1 AND $2`, base, base+10) })
	for k, l := range f.Leads {
		var id int64
		fmt.Sscan(k, &id)
		if _, err := e.owner.Exec(ctx, `INSERT INTO leads (id, workspace_id, company, contact_person, specialization, city, email, linkedin_url)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, base+id, e.ws, l["company"], l["contact_person"], l["specialization"], l["city"],
			l["email"], l["linkedin_url"]); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range f.Suggest {
		var a map[string]any
		_ = json.Unmarshal(c.Args, &a)
		if id, ok := a["lead_id"].(float64); ok {
			a["lead_id"] = base + int64(id)
		}
		c.Args, _ = json.Marshal(a)
		got, handled, err := e.run(t, c, &Engine{})
		checkResult(t, e, c, got, handled, err)
	}
	for _, c := range f.Draft {
		got, handled, err := e.run(t, c, &Engine{})
		checkResult(t, e, c, got, handled, err)
	}
	var sent int
	if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM outreach_drafts WHERE workspace_id = $1 AND (sent_at IS NOT NULL OR state <> 'draft')`, e.ws).Scan(&sent); err != nil || sent != 0 {
		t.Fatalf("a draft left the draft state: %d %v", sent, err)
	}
}

func TestDraftToolRefusesSuppressedAddress(t *testing.T) {
	var f struct {
		Draft []toolCase `json:"draft"`
	}
	loadTools(t, "tools_outreach.json", &f)
	e := toolsSetup(t)
	ctx := context.Background()
	if _, err := e.owner.Exec(ctx, `INSERT INTO outreach_suppressions (workspace_id, email, reason, source) VALUES ($1, 'ann.lee@acme.com', 'unsubscribe', 'test')`, e.ws); err != nil {
		t.Skip("suppression table shape: ", err)
	}
	t.Cleanup(func() { _, _ = e.owner.Exec(ctx, `DELETE FROM outreach_suppressions WHERE workspace_id = $1`, e.ws) })
	c := f.Draft[2] // first successful draft (person_a)
	got, _, err := e.run(t, c, &Engine{})
	if err != nil || !strings.Contains(got, `"ok": false`) || !strings.Contains(got, "suppressed_address") {
		t.Fatalf("got %s %v", got, err)
	}
}

func TestPlanToolsMatchPython(t *testing.T) {
	var f struct {
		Draft    []toolCase `json:"draft"`
		Describe []struct {
			Args        map[string]any `json:"args"`
			WorkspaceID string         `json:"workspace_id"`
			Text        string         `json:"text"`
		} `json:"describe"`
	}
	loadTools(t, "tools_plan.json", &f)
	e := toolsSetup(t)
	ctx := context.Background()
	hex32 := regexp.MustCompile(`\b[0-9a-f]{32}\b`)
	eng := &Engine{Memory: &memRecorder{}}
	var first map[string]any
	for _, c := range f.Draft {
		got, handled, err := e.run(t, c, eng)
		if err != nil || !handled {
			t.Fatalf("%s: %v", c.Args, err)
		}
		if g, w := hex32.ReplaceAllString(got, "$HEX"), hex32.ReplaceAllString(c.Result, "$HEX"); g != w {
			t.Fatalf("%s:\n got %s\nwant %s", c.Args, g, w)
		}
		if first == nil {
			_ = json.Unmarshal([]byte(got), &first)
		}
	}
	pid, nonce := first["plan_id"].(string), first["nonce"].(string)
	// the confirmation card renders the stored plan, never the client body
	tn := e.turn()
	tn.UID = i32(2)
	tn.Engine = eng
	if d := tn.describe(ctx, parsedCall{Name: "execute_plan", Args: map[string]any{"plan_id": pid}}); d != f.Describe[0].Text {
		t.Fatalf("describe stored:\n got %q\nwant %q", d, f.Describe[0].Text)
	}
	if d := tn.describe(ctx, parsedCall{Name: "execute_plan", Args: plainCopy(f.Describe[2].Args).(map[string]any)}); d != f.Describe[2].Text {
		t.Fatalf("describe client body: %q", d)
	}
	for _, i := range []int{3, 4} {
		if d := tn.describe(ctx, parsedCall{Name: "execute_plan", Args: plainCopy(f.Describe[i].Args).(map[string]any)}); d != f.Describe[i].Text {
			t.Fatalf("describe %d: %q", i, d)
		}
	}
	other := e.turn()
	other.WS = e.other
	other.Engine = eng
	if d := other.describe(ctx, parsedCall{Name: "execute_plan", Args: map[string]any{"plan_id": pid}}); d != f.Describe[1].Text {
		t.Fatalf("describe from another workspace: %q", d)
	}
	exec := func(uid int32, args map[string]any) string {
		tn := e.turn()
		tn.UID = i32(uid)
		tn.Engine = eng
		got, handled, err := tn.engineTool(ctx, parsedCall{Name: "execute_plan", Args: args})
		if err != nil || !handled {
			t.Fatalf("execute: %v", err)
		}
		return got
	}
	notFound := `{"error": "Plan not found or already executed. Re-draft and approve the plan."}`
	if got := exec(2, map[string]any{"plan_id": pid, "nonce": "wrong"}); got != notFound {
		t.Fatalf("wrong nonce: %s", got)
	}
	if got := exec(1, map[string]any{"plan_id": pid, "nonce": nonce}); got != notFound { // another (admin) user
		t.Fatalf("other user: %s", got)
	}
	if got := exec(3, map[string]any{"plan_id": pid, "nonce": nonce}); got != errInsufficientRole {
		t.Fatalf("viewer: %s", got)
	}
	got := exec(2, map[string]any{"plan_id": pid, "nonce": nonce})
	var res map[string]any
	_ = json.Unmarshal([]byte(got), &res)
	steps, _ := res["steps_done"].([]any)
	if res["ok"] != true || res["workbook_id"] == nil || len(steps) != 2 {
		t.Fatalf("execute: %s", got)
	}
	var key string
	if err := e.owner.QueryRow(ctx, `SELECT source_type FROM workbooks WHERE id = $1 AND workspace_id = $2`, res["workbook_id"], e.ws).Scan(&key); err != nil {
		t.Fatalf("plan workbook missing: %v", err)
	}
	if got := exec(2, map[string]any{"plan_id": pid, "nonce": nonce}); got != notFound {
		t.Fatalf("second execution: %s", got)
	}
	if got := exec(2, map[string]any{"plan": first["plan"]}); got != notFound {
		t.Fatalf("client plan body executed: %s", got)
	}
	if m := eng.Memory.(*memRecorder); len(m.recs) != 1 || m.recs[0] != "ok" {
		t.Fatalf("memory %v", m.recs)
	}
}

type memRecorder struct{ recs []string }

func (m *memRecorder) Recent(context.Context, string, int) ([]autopilot.MemoryRecord, error) {
	return nil, nil
}

func (m *memRecorder) Record(_ context.Context, ws, goal string, wb *string, outcome string) error {
	m.recs = append(m.recs, outcome)
	return nil
}

func TestPeoplePreRoutesMatchPython(t *testing.T) {
	var f struct {
		Seq   []string    `json:"uuid_hex_sequence"`
		Cases []preVector `json:"cases"`
	}
	readVectors(t, "tools_preroute.json", &f)
	var people struct {
		Research map[string]any `json:"research_result"`
		Verify   map[string]any `json:"verify_result"`
	}
	loadTools(t, "tools_people.json", &people)
	e := persistSetup(t)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = e.owner.Exec(ctx, `DELETE FROM chat_tool_approvals WHERE workspace_id = $1`, e.ws) })
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			e.reset(t)
			_, _ = e.owner.Exec(ctx, `DELETE FROM chat_tool_approvals WHERE workspace_id = $1`, e.ws)
			for name, res := range c.Priors {
				td, _ := json.Marshal(map[string]any{"name": name, "result": res})
				s := string(td)
				if err := tenancy.InWorkspace(ctx, e.pool, e.ws, func(tx pgx.Tx) error {
					return chathistory.AddMessage(ctx, tx, e.conv, "tool", name+" result", &s, e.ws, i32(7))
				}); err != nil {
					t.Fatal(err)
				}
			}
			n := 0
			fp := &fakePeople{research: people.Research, verify: people.Verify}
			tn := e.turn()
			tn.Engine = &Engine{People: fp, IntentPollerEnabled: true, PGLeadStore: true,
				NewID: func() string { n++; return f.Seq[n-1] }}
			rec := newRec()
			w, _ := ssestream.New(rec, nil)
			handled, delegate, err := tn.nativePreRoute(ctx, w, c.Message, false)
			if err != nil || !handled || delegate {
				t.Fatalf("handled=%v delegate=%v err=%v", handled, delegate, err)
			}
			var approvals []struct{ ID, Call string }
			rows, err := e.owner.Query(ctx, `SELECT id, tool_call_json FROM chat_tool_approvals WHERE workspace_id = $1 ORDER BY created_at_epoch, id`, e.ws)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var a struct{ ID, Call string }
				_ = rows.Scan(&a.ID, &a.Call)
				approvals = append(approvals, a)
			}
			rows.Close()
			norm := func(s string) string {
				s = strings.ReplaceAll(s, e.conv, "conv-1")
				for i, a := range approvals {
					s = strings.ReplaceAll(s, a.ID, "appr-"+string(rune('1'+i)))
				}
				return s
			}
			var wantLines []string
			var persisted []string
			for _, fr := range c.Frames {
				switch fr.T {
				case "sse":
					wantLines = append(wantLines, fr.Line)
				case "persist":
					persisted = append(persisted, fr.Role+"|"+fr.Content)
				}
			}
			gotLines := strings.SplitAfter(norm(rec.String()), "\n\n")
			gotLines = gotLines[:len(gotLines)-1]
			if len(gotLines) != len(wantLines) {
				t.Fatalf("frames:\n got %q\nwant %q", gotLines, wantLines)
			}
			for i := range wantLines {
				g, w := gotLines[i], wantLines[i]
				if g == w {
					continue
				}
				if !strings.HasPrefix(g, "data: {") || !reflect.DeepEqual(sem(t, strings.TrimPrefix(strings.TrimSpace(g), "data: ")), sem(t, strings.TrimPrefix(strings.TrimSpace(w), "data: "))) {
					t.Fatalf("frame %d:\n got %q\nwant %q", i, g, w)
				}
			}
			ms := e.messages(t)
			if len(ms) != len(persisted)+len(c.Priors) {
				t.Fatalf("persisted %d rows, want %d", len(ms)-len(c.Priors), len(persisted))
			}
			for i, m := range ms[len(c.Priors):] {
				if got := m.Role + "|" + m.Content; got != persisted[i] {
					t.Errorf("row %d: %q, want %q", i, got, persisted[i])
				}
			}
			if len(approvals) != len(c.Approvals) {
				t.Fatalf("approvals %d, want %d", len(approvals), len(c.Approvals))
			}
			for i, a := range c.Approvals {
				wb, _ := canonjson.Marshal(normalizeJSON(t, a.ToolCall), true)
				if got := norm(approvals[i].Call); got != string(wb) {
					t.Errorf("approval %d:\n got %s\nwant %s", i, got, wb)
				}
			}
		})
	}
}

// TestPeopleWorkbookResolvesPersonsLikePython runs the workbook cases with the real entities.ResolvePerson and
// compares the person graph with the one Python wrote.
func TestPeopleWorkbookResolvesPersonsLikePython(t *testing.T) {
	var f struct {
		Research  map[string]any `json:"research_result"`
		Verify    map[string]any `json:"verify_result"`
		Workbooks []toolCase     `json:"workbooks"`
		DB        struct {
			Identifiers [][]any `json:"person_identifiers"`
			Entities    [][]any `json:"person_entities"`
			Employments [][]any `json:"person_employments"`
		} `json:"db"`
	}
	loadTools(t, "tools_people.json", &f)
	e := toolsSetup(t)
	ctx := context.Background()
	clean := func() {
		for _, tb := range []string{"person_employments", "person_identifiers", "person_entities"} {
			_, _ = e.owner.Exec(ctx, `DELETE FROM `+tb+` WHERE workspace_id = $1`, e.ws)
		}
	}
	clean()
	t.Cleanup(clean)
	eng := &Engine{People: &fakePeople{research: f.Research, verify: f.Verify},
		ResolvePerson: func(ctx context.Context, tx pgx.Tx, ws string, o PersonObservation) (string, error) {
			id, _, err := entities.ResolvePerson(ctx, tx, ws, entities.PersonObservation{Name: o.Name, Company: o.Company,
				Source: o.Source, Title: o.Title, CompanyDomain: o.CompanyDomain, LinkedInURL: o.LinkedInURL, Email: o.Email,
				EvidenceURL: o.EvidenceURL, LegacyIDs: o.LegacyIDs})
			return id, err
		}}
	for _, c := range f.Workbooks {
		got, handled, err := e.run(t, c, eng)
		checkResult(t, e, c, got, handled, err)
	}
	rows := func(sql string) string {
		var s string
		if err := e.owner.QueryRow(ctx, `SELECT coalesce(json_agg(x), '[]')::text FROM (`+sql+`) r(x)`, e.ws).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	want := func(v [][]any) string {
		var out []string
		for _, r := range v {
			b, _ := json.Marshal(r)
			out = append(out, string(b))
		}
		sort.Strings(out)
		return strings.Join(out, "\n")
	}
	got := func(s string) string {
		var l [][]any
		_ = json.Unmarshal([]byte(s), &l)
		for _, r := range l {
			for i, x := range r {
				r[i] = semantic(x)
			}
		}
		return want(l)
	}
	if g, w := got(rows(`SELECT json_build_array(kind, value) FROM person_identifiers WHERE workspace_id = $1`)), want(f.DB.Identifiers); g != w {
		t.Fatalf("identifiers:\n got %s\nwant %s", g, w)
	}
	if g, w := got(rows(`SELECT json_build_array(company_key, company_name, company_domain, title, source, evidence_url, is_current)
		FROM person_employments WHERE workspace_id = $1`)), want(f.DB.Employments); g != w {
		t.Fatalf("employments:\n got %s\nwant %s", g, w)
	}
	if g, w := got(rows(`SELECT json_build_array(full_name, identity_keys, fields, corroboration_count) FROM person_entities WHERE workspace_id = $1`)),
		want(f.DB.Entities); g != w {
		t.Fatalf("entities:\n got %s\nwant %s", g, w)
	}
}
