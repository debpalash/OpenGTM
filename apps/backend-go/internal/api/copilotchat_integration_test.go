//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chathistory"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chatmemory"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/copilot"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ratelimit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

type chatRecordedRPC struct {
	RPC      string            `json:"rpc"`
	Request  map[string]any    `json:"request"`
	Status   int               `json:"status"`
	Frames   []json.RawMessage `json:"frames"`
	Response map[string]any    `json:"response"`
}

func chatLoadRPCs(t *testing.T, name string) []chatRecordedRPC {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/copilot/frames/" + name + ".ndjson")
	if err != nil {
		t.Fatal(err)
	}
	var out []chatRecordedRPC
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r chatRecordedRPC
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// chatFake scripts the llm rounds (copilot.Turn.Rounds, set by newChatEnv's configure hook): it replays the
// recorded llm rounds in order and records each round request with the keys the recordings use.
type chatFake struct {
	t       *testing.T
	mu      sync.Mutex
	llm     [][]json.RawMessage
	llmReqs []map[string]any
	// behaviours
	llmFail  bool          // the round cannot open
	llmHold  chan struct{} // the round stalls until released or the turn ends
	llmStart chan struct{}
	llmDone  chan struct{}
}

func newChatFake(t *testing.T) *chatFake {
	return &chatFake{t: t, llmStart: make(chan struct{}, 16), llmDone: make(chan struct{}, 16)}
}

var errChatFakeRound = errors.New("chat fake: provider unreachable")

// chatSrc replays one round's frames; when hold is set it first blocks until released or cancelled.
type chatSrc struct {
	ctx    context.Context
	f      *chatFake
	hold   chan struct{}
	lines  []json.RawMessage
	closed chan struct{}
	once   sync.Once
}

func (s *chatSrc) Close() { s.once.Do(func() { close(s.closed) }) }

func (s *chatSrc) Next() (copilot.Frame, error) {
	if s.hold != nil {
		select {
		case <-s.hold:
			s.hold = nil
		case <-s.ctx.Done():
			s.f.llmDone <- struct{}{}
			return copilot.Frame{}, s.ctx.Err()
		case <-s.closed:
			s.f.llmDone <- struct{}{}
			return copilot.Frame{}, errChatFakeRound
		}
	}
	if len(s.lines) == 0 {
		return copilot.Frame{}, errChatFakeRound // no terminal frame
	}
	l := s.lines[0]
	s.lines = s.lines[1:]
	var w struct {
		T            string          `json:"t"`
		D            json.RawMessage `json:"d"`
		Cursor       *int            `json:"cursor"`
		ProviderDone *bool           `json:"provider_done"`
	}
	if err := json.Unmarshal(l, &w); err != nil {
		return copilot.Frame{}, err
	}
	var cb bytes.Buffer
	d := w.D
	if len(d) > 0 && json.Compact(&cb, d) == nil {
		d = cb.Bytes()
	}
	return copilot.Frame{T: w.T, D: d, Cursor: w.Cursor, ProviderDone: w.ProviderDone}, nil
}

func (f *chatFake) round(ws string) copilot.RoundFunc {
	return func(ctx context.Context, rq copilot.RoundRequest) (copilot.FrameSource, error) {
		b, _ := json.Marshal(map[string]any{"workspace_id": ws, "messages": rq.Messages, "tools_enabled": rq.ToolsEnabled,
			"provider_cursor": rq.Cursor, "round_budget_hit": rq.BudgetHit})
		var req map[string]any
		_ = json.Unmarshal(b, &req)
		f.mu.Lock()
		f.llmReqs = append(f.llmReqs, req)
		var lines []json.RawMessage
		if len(f.llm) > 0 {
			lines, f.llm = f.llm[0], f.llm[1:]
		}
		fail, hold := f.llmFail, f.llmHold
		f.mu.Unlock()
		f.llmStart <- struct{}{}
		if fail {
			return nil, errChatFakeRound
		}
		if lines == nil {
			lines = []json.RawMessage{json.RawMessage(`{"t":"content","d":"ok"}`), json.RawMessage(`{"t":"done","cursor":0}`)}
		}
		return &chatSrc{ctx: ctx, f: f, hold: hold, lines: lines, closed: make(chan struct{})}, nil
	}
}

// script loads the recorded llm rounds of a scenario (the pre-route runs in Go).
func (f *chatFake) script(name string) {
	for _, r := range chatLoadRPCs(f.t, name) {
		if r.RPC == "llm" {
			f.llm = append(f.llm, r.Frames)
		}
	}
}

func (f *chatFake) llmRequests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.llmReqs...)
}

type chatSettingsMap map[string]string

func (s chatSettingsMap) Get(_ context.Context, key, def string) (string, error) {
	if v, ok := s[key]; ok {
		return v, nil
	}
	return def, nil
}

type chatEnv struct {
	leadsEnv
	fake  *chatFake
	hnd   *chatHandler
	caps  *ssestream.Caps
	logs  *chatSyncBuf
	alice int32
	bob   int32
}

type chatSyncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *chatSyncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *chatSyncBuf) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func chatPurge() {
	for _, q := range []string{
		`DELETE FROM chat_messages WHERE workspace_id IN ('ws-alpha','ws-beta')`,
		`DELETE FROM chat_conversations WHERE workspace_id IN ('ws-alpha','ws-beta')`,
		`DELETE FROM chat_tool_approvals WHERE workspace_id IN ('ws-alpha','ws-beta')`,
		`DELETE FROM chat_memories WHERE user_id LIKE 'ws-alpha:%' OR user_id LIKE 'ws-beta:%'`,
	} {
		wcrudOwnerExecQuiet(q)
	}
}

type chatOpts struct {
	maxBody    int
	perUser    int
	configure  func(*copilot.Turn)
	rounds3    bool
	maxStreams int
	// final security M3
	limiter                *ratelimit.Limiter
	perUserRate, perWSRate int
	budgetUSD              float64
}

func newChatEnv(t *testing.T, o chatOpts) *chatEnv {
	t.Helper()
	le := newLeadsEnv(t)
	chatPurge()
	t.Cleanup(chatPurge)
	f := newChatFake(t)
	logs := &chatSyncBuf{}
	perUser := o.perUser
	if perUser <= 0 {
		perUser = 3
	}
	caps := ssestream.NewCaps(max(o.maxStreams, 8), perUser)
	cs := config.CopilotSettings{GoCopilotEnabled: true, ChatRequireAuth: true, MaxBodyBytes: 1 << 20, MaxStreamsPerUser: 3,
		TurnTimeoutSeconds: 30, ToolTimeoutSeconds: 10}
	if o.maxBody > 0 {
		cs.MaxBodyBytes = o.maxBody
	}
	cs.ChatPerUserPerMinute, cs.ChatPerWorkspacePerMinute, cs.LLMDailyBudgetUSD = o.perUserRate, o.perWSRate, o.budgetUSD
	env, err := secrets.NewEnvelope(secrets.Config{MasterKey: "copilot-chat-master", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Pool: le.pool, Signer: testSigner, Identity: le.st, Flags: config.Flags{PGLeadStore: true, Copilot: cs},
		ChatStreams: caps, Registry: testRegistry(t), Envelope: env, Limiter: o.limiter,
		Log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	h := newChatHandler(d)
	if !h.native {
		t.Fatalf("chat engine not built: %s", logs.String())
	}
	configure := o.configure
	h.configure = func(tn *copilot.Turn) {
		tn.Rounds = f.round(tn.WS)
		if o.rounds3 {
			tn.Settings = chatSettingsMap{"CHAT_MAX_TOOL_ROUNDS": "3"}
		}
		if configure != nil {
			configure(tn)
		}
	}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(le.pool, nil))
	r.Handle("POST /api/copilotkit", h)
	le.h = r.Handler()
	e := &chatEnv{leadsEnv: le, fake: f, hnd: h, caps: caps, logs: logs}
	e.alice = le.mustUserByName(t, "alice").ID
	e.bob = le.mustUserByName(t, "bob").ID
	// conversations of the recording seed (contract_seed.py)
	for _, c := range []struct {
		id, ws string
		uid    int32
	}{{"conv-ed-1", "ws-alpha", e.alice}, {"conv-ed-2", "ws-alpha", e.alice}, {"conv-ed-3", "ws-alpha", e.alice}, {"conv-b-1", "ws-beta", e.bob}} {
		ownerExec(t, `INSERT INTO chat_conversations (id, title, workspace_id, user_id) VALUES ($1,'seed',$2,$3)`, c.id, c.ws, fmt.Sprint(c.uid))
	}
	return e
}

func (e *chatEnv) post(who, ws, body string) *httptest.ResponseRecorder {
	return e.send(who, ws, "POST", "/api/copilotkit", body)
}

var chatIdem = regexp.MustCompile(`(chat-[a-z]+:)[^"\\]+`)

var chatHex32 = regexp.MustCompile(`[0-9a-f]{32}`)

func chatFrames(body string) []string {
	var out []string
	for _, p := range strings.Split(body, "\n\n") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// chatConvOf returns the conversation id of the first frame.
func chatConvOf(t *testing.T, body string) string {
	t.Helper()
	fs := chatFrames(body)
	if len(fs) == 0 || !strings.HasPrefix(fs[0], `data: {"conversation_id": "`) {
		t.Fatalf("first frame is not conversation_id: %q", body)
	}
	return strings.TrimSuffix(strings.TrimPrefix(fs[0], `data: {"conversation_id": "`), `"}`)
}

func chatNorm(body string) string {
	fs := chatFrames(body)
	if len(fs) > 0 && strings.HasPrefix(fs[0], `data: {"conversation_id"`) {
		fs[0] = `data: {"conversation_id": "<conv>"}`
	}
	for i, f := range fs {
		if strings.HasPrefix(f, `data: {"tool_result"`) {
			// Native tool results come from the live database: the key order of a GROUP BY map (get_lead_stats)
			// is whatever the store returned, so results compare as JSON values (the exact bytes of recorded
			// tool results are pinned in internal/copilot).
			dec := json.NewDecoder(strings.NewReader(strings.TrimPrefix(f, "data: ")))
			dec.UseNumber()
			var v any
			if dec.Decode(&v) == nil {
				b, _ := json.Marshal(v)
				fs[i] = "data: " + string(b)
			}
		}
	}
	out := chatHex32.ReplaceAllString(strings.Join(fs, "\n\n"), "<id>")
	// idempotency keys embed the conversation id and the people selection id of the recording run; the fake
	// worker replays frames recorded under other ids
	return chatIdem.ReplaceAllString(out, "${1}<key>")
}

type chatContractCase struct {
	body   string
	req    map[string]any
	expect string
}

func chatLoadContract(t *testing.T, name string) chatContractCase {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/contract/copilot/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Request struct {
			Body json.RawMessage `json:"body"`
		} `json:"request"`
		Response struct {
			Status int    `json:"status"`
			Body   string `json:"body"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	_ = json.Unmarshal(c.Request.Body, &req)
	return chatContractCase{body: string(c.Request.Body), req: req, expect: c.Response.Body}
}

// chatDescsOf collects the confirmation descriptions of a recorded body (the recorder ran the real describe).
func chatDescsOf(expect string) map[string]string {
	out := map[string]string{}
	for _, f := range chatFrames(expect) {
		var m map[string]json.RawMessage
		if !strings.HasPrefix(f, "data: {") || json.Unmarshal([]byte(f[6:]), &m) != nil || m["confirmation_required"] == nil {
			continue
		}
		var cr struct{ Name, Description string }
		_ = json.Unmarshal(m["confirmation_required"], &cr)
		out[cr.Name] = cr.Description
	}
	return out
}

// chatSameLLM compares the llm rounds the route sent with the recorded ones. The memory block depends on the
// state of the database, so it is checked by its own test.
func chatSameLLM(t *testing.T, name string, got []map[string]any, rec []chatRecordedRPC) {
	t.Helper()
	var want []map[string]any
	for _, r := range rec {
		if r.RPC == "llm" {
			want = append(want, r.Request)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%s: %d llm rounds, recorded %d", name, len(got), len(want))
	}
	// Final security Low 8: every executed tool result reaches the model fenced as untrusted data (our own
	// denial messages are not tool output and stay bare). The recorder's stubbed contents are compared below.
	for _, g := range got {
		for _, x := range g["messages"].([]any) {
			mm := x.(map[string]any)
			c, _ := mm["content"].(string)
			if mm["role"] == "tool" && !strings.HasPrefix(c, `{"denied": true`) && !strings.HasPrefix(c, copilot.UntrustedToolNotice+"\n<<<BEGIN_UNTRUSTED_") {
				t.Fatalf("%s: tool message not fenced: %.200q", name, c)
			}
		}
	}
	strip := func(m map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range m {
			out[k] = v
		}
		var ms []any
		for i, x := range m["messages"].([]any) {
			mm := x.(map[string]any)
			if i == 0 && mm["role"] == "system" { // the recorder stubbed the prompt; Go builds it (pinned in internal/copilot)
				mm = map[string]any{"role": "system", "content": "SYSTEM"}
				x = mm
			}
			if c, _ := mm["content"].(string); mm["role"] == "system" && strings.HasPrefix(c, "Relevant memories from past conversations:") {
				continue
			}
			if mm["role"] == "tool" { // the recorder stubbed native tool results; the route ran them for real
				cp := map[string]any{}
				for k, v := range mm {
					cp[k] = v
				}
				cp["content"] = "<tool result>"
				x = cp
			}
			ms = append(ms, x)
		}
		out["messages"] = ms
		return out
	}
	for i := range got {
		g := chatRoundTrip(t, got[i])
		w := chatRoundTrip(t, want[i])
		if !reflect.DeepEqual(strip(g), strip(w)) {
			gb, _ := json.Marshal(strip(g))
			wb, _ := json.Marshal(strip(w))
			t.Errorf("%s: llm request %d differs\n got %s\nwant %s", name, i, gb, wb)
		}
	}
}

func chatRoundTrip(t *testing.T, v map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestChatBytesMatchLegacyForEveryScenario(t *testing.T) {
	var names []string
	for _, n := range contractFixtureNames(t, "copilot") {
		n = strings.TrimPrefix(n, "copilot/")
		// The pre-route branches (pre_*) run in Go and are pinned against the Python recordings in internal/copilot
		// (native pre-route and people vectors); they need the people service, so only chat_* replays here.
		if strings.HasPrefix(n, "chat_") {
			names = append(names, n)
		}
	}
	if len(names) < 20 {
		t.Fatalf("only %d recorded chat scenarios found", len(names))
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			e := newChatEnv(t, chatOpts{rounds3: strings.Contains(name, "tool_loop_budget")})
			c := chatLoadContract(t, name)
			e.fake.script(name)
			rec := e.post("alice", "ws-alpha", c.body)
			if rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream; charset=utf-8" {
				t.Fatalf("content-type %q", ct)
			}
			for k, v := range map[string]string{"Cache-Control": "no-cache", "Connection": "keep-alive", "X-Accel-Buffering": "no"} {
				if rec.Header().Get(k) != v {
					t.Fatalf("header %s = %q", k, rec.Header().Get(k))
				}
			}
			got := rec.Body.String()
			if chatNorm(got) != chatNorm(c.expect) {
				t.Fatalf("bytes differ\n got %q\nwant %q", chatNorm(got), chatNorm(c.expect))
			}
			if id, _ := c.req["conversation_id"].(string); strings.HasPrefix(id, "conv-ed-") && chatConvOf(t, got) != id {
				t.Fatalf("an owned conversation id must be kept: %s", chatConvOf(t, got))
			}
			chatSameLLM(t, name, e.fake.llmRequests(), chatLoadRPCs(t, name))
		})
	}
}

func (e *chatEnv) convCount(t *testing.T, ws string) int {
	t.Helper()
	return chatCount(t, `SELECT count(*) FROM chat_conversations WHERE workspace_id = '`+ws+`'`)
}

func (e *chatEnv) rows(t *testing.T, conv string) []chathistory.Message {
	t.Helper()
	var out []chathistory.Message
	err := tenancy.InWorkspace(context.Background(), e.pool, "ws-alpha", func(tx pgx.Tx) error {
		var err error
		out, err = chathistory.Messages(context.Background(), tx, conv, "ws-alpha", &e.alice)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestChatConversationCreatedBeforeStream(t *testing.T) {
	e := newChatEnv(t, chatOpts{})
	e.fake.script("chat_text_plain")
	before := e.convCount(t, "ws-alpha")
	rec := e.post("alice", "ws-alpha", `{"messages":[{"role":"user","content":"[sim:text_plain] Say hello"}]}`)
	id := chatConvOf(t, rec.Body.String())
	if e.convCount(t, "ws-alpha") != before+1 {
		t.Fatal("conversation not created")
	}
	ms := e.rows(t, id)
	if len(ms) != 2 || ms[0].Role != "user" || ms[0].Content != "[sim:text_plain] Say hello" || ms[1].Role != "assistant" {
		t.Fatalf("rows %+v", ms)
	}
	if !strings.HasPrefix(ms[1].Content, "Hello from the simulator") {
		t.Fatalf("assistant row %q", ms[1].Content)
	}
	if n := len(e.fake.llmRequests()); n != 1 {
		t.Fatalf("llm rounds %d", n)
	}
	var title string
	_ = wcrudOwnerQueryRow(t, `SELECT title FROM chat_conversations WHERE id = $1`, id).Scan(&title)
	if title != "[sim:text_plain] Say hello" {
		t.Fatalf("title %q", title)
	}
	mems := e.memoriesOf(t)
	if len(mems) != 1 || !strings.HasPrefix(mems[0].Memory, "User asked: [sim:text_plain] Say hello\nAssistant answered about: Hello") ||
		mems[0].Metadata["type"] != "episodic" || mems[0].Metadata["conversation_id"] != id {
		t.Fatalf("memory %+v", mems)
	}
}

func (e *chatEnv) memoriesOf(t *testing.T) []chatmemory.Memory {
	t.Helper()
	ns, _ := chatmemory.Namespace("ws-alpha", &e.alice)
	var out []chatmemory.Memory
	err := tenancy.InWorkspace(context.Background(), e.pool, "ws-alpha", func(tx pgx.Tx) error {
		var err error
		out, err = chatmemory.All(context.Background(), tx, ns)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestChatPersistsToolRows(t *testing.T) {
	e := newChatEnv(t, chatOpts{})
	e.fake.script("chat_tool_one")
	rec := e.post("alice", "ws-alpha", `{"messages":[{"role":"user","content":"[sim:tool_one] Show me acme leads"}]}`)
	ms := e.rows(t, chatConvOf(t, rec.Body.String()))
	if len(ms) != 3 || ms[1].Role != "tool" || ms[1].Content != "search_leads result" || ms[1].ToolData == nil ||
		!strings.HasPrefix(*ms[1].ToolData, `{"name": "search_leads", "result": {"leads": [`) || ms[2].Role != "assistant" || ms[2].Content != "Found the leads. Done." {
		t.Fatalf("rows %+v", ms)
	}
}

func TestChatForgedConversationIDStartsFresh(t *testing.T) {
	e := newChatEnv(t, chatOpts{})
	e.fake.script("chat_conv_foreign")
	rec := e.post("alice", "ws-alpha", `{"conversation_id":"conv-b-1","messages":[{"role":"user","content":"[sim:text_plain] continue please"}]}`)
	id := chatConvOf(t, rec.Body.String())
	if id == "conv-b-1" {
		t.Fatal("a foreign conversation id was adopted")
	}
	if n := chatCount(t, `SELECT count(*) FROM chat_messages WHERE conversation_id = 'conv-b-1'`); n != 0 {
		t.Fatalf("a foreign conversation got %d messages", n)
	}
	if len(e.rows(t, id)) != 2 {
		t.Fatal("fresh conversation has no history")
	}
	// the same id in the right workspace but as another user is foreign too
	e.fake.script("chat_conv_foreign")
	rec = e.post("alice", "ws-alpha", `{"conversation_id":"conv-ed-1","messages":[{"role":"user","content":"x"}]}`)
	if chatConvOf(t, rec.Body.String()) != "conv-ed-1" {
		t.Fatal("an owned conversation is continued")
	}
}

func TestChatUnauthenticated401NoSideEffects(t *testing.T) {
	e := newChatEnv(t, chatOpts{})
	before := chatCount(t, `SELECT count(*) FROM chat_conversations WHERE workspace_id IN ('ws-alpha','ws-beta')`)
	for _, hdr := range []map[string]string{nil, {"Authorization": "Bearer garbage"}} {
		rec, _ := e.do("POST", "/api/copilotkit", "application/json", `{"messages":[{"role":"user","content":"hi"}]}`, hdr)
		if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "Bearer" || !strings.Contains(rec.Body.String(), "Could not validate credentials") {
			t.Fatalf("%d %q", rec.Code, rec.Body.String())
		}
	}
	if chatCount(t, `SELECT count(*) FROM chat_conversations WHERE workspace_id IN ('ws-alpha','ws-beta')`) != before || chatCount(t, `SELECT count(*) FROM chat_messages WHERE workspace_id IN ('ws-alpha','ws-beta')`) != 0 {
		t.Fatal("an unauthenticated request left a row")
	}
	if len(e.fake.llmRequests()) != 0 || e.caps.Active() != 0 {
		t.Fatal("an unauthenticated request reached the llm")
	}
	rec := e.post("alice", "ws-beta", `{"messages":[]}`) // alice is no member of ws-beta
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "Workspace access denied") {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

func TestChatInvalidBody422(t *testing.T) {
	e := newChatEnv(t, chatOpts{})
	for _, body := range []string{`{`, `[]`, `{"messages":"x"}`, `{"messages":["x"]}`, `{"messages":[{"role":"user","content":3}]}`,
		`{"messages":[]} trailing`, `{"approved_tool_calls":"x"}`} {
		rec := e.post("alice", "ws-alpha", body)
		if rec.Code != 422 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s: %d %q", body, rec.Code, rec.Body.String())
		}
	}
	if chatCount(t, `SELECT count(*) FROM chat_conversations WHERE workspace_id IN ('ws-alpha','ws-beta') AND title <> 'seed'`) != 0 || e.caps.Active() != 0 {
		t.Fatal("an invalid body left a row or a slot")
	}
}

func TestChatBodyTooLarge413(t *testing.T) {
	e := newChatEnv(t, chatOpts{maxBody: 2048})
	big := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 4096) + `"}]}`
	rec := e.post("alice", "ws-alpha", big)
	if rec.Code != 413 {
		t.Fatalf("%d", rec.Code)
	}
	if chatCount(t, `SELECT count(*) FROM chat_conversations WHERE workspace_id IN ('ws-alpha','ws-beta') AND title <> 'seed'`) != 0 {
		t.Fatal("an oversized body left a conversation")
	}
}

func TestChatTooManyMessages422(t *testing.T) {
	e := newChatEnv(t, chatOpts{})
	body := `{"messages":[` + strings.Repeat(`{"role":"user","content":"x"},`, 200) + `{"role":"user","content":"x"}]}`
	if rec := e.post("alice", "ws-alpha", body); rec.Code != 422 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestChatTooManyApprovals400(t *testing.T) {
	e := newChatEnv(t, chatOpts{})
	body := `{"approved_tool_calls":[` + strings.Repeat(`{"tool_call":{"id":"x"}},`, 20) + `{"tool_call":{"id":"x"}}],"messages":[]}`
	rec := e.post("alice", "ws-alpha", body)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Too many approved tool calls") {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if len(e.fake.llmRequests()) != 0 {
		t.Fatal("reached the worker")
	}
}

func (e *chatEnv) propose(t *testing.T, uid int32, name, args string) string {
	t.Helper()
	var id string
	err := tenancy.InWorkspace(context.Background(), e.pool, "ws-alpha", func(tx pgx.Tx) error {
		var err error
		id, err = chathistory.CreateApproval(context.Background(), tx, "ws-alpha", &uid, map[string]any{
			"id": "call_x", "type": "function", "function": map[string]any{"name": name, "arguments": args}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *chatEnv) leadStatus(t *testing.T, id int) string {
	t.Helper()
	var s string
	if err := wcrudOwnerQueryRow(t, `SELECT status FROM leads WHERE workspace_id='ws-alpha' AND id=$1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestChatApprovalResubmitFlow(t *testing.T) {
	e := newChatEnv(t, chatOpts{})
	args := `{"lead_id": 4, "status": "qualified"}`
	before := e.leadStatus(t, 4)
	if before == "qualified" {
		t.Skip("seed lead 4 is already qualified")
	}
	run := func(name, id, decisionJSON string) string {
		c := chatLoadContract(t, name)
		e.fake.script(name)
		body := strings.NewReplacer(`"apr-pending-1"`, `"`+id+`"`, `"apr-pending-2"`, `"`+id+`"`, `"apr-consumed-1"`, `"`+id+`"`).Replace(c.body)
		if decisionJSON != "" {
			body = regexp.MustCompile(`"decision": "[a-z]+"`).ReplaceAllString(body, decisionJSON)
		}
		rec := e.post("alice", "ws-alpha", body)
		if rec.Code != 200 || chatNorm(rec.Body.String()) != chatNorm(c.expect) {
			t.Fatalf("%s: %d\n got %q\nwant %q", name, rec.Code, chatNorm(rec.Body.String()), chatNorm(c.expect))
		}
		return rec.Body.String()
	}
	id := e.propose(t, e.alice, "update_lead_status", args)
	run("approval_approve", id, "")
	if e.leadStatus(t, 4) != "qualified" {
		t.Fatal("the approved call did not run")
	}
	ownerExec(t, `UPDATE leads SET status = $1 WHERE workspace_id='ws-alpha' AND id = 4`, before)
	run("approval_replay", id, "") // single use: nothing runs the second time
	if e.leadStatus(t, 4) != before {
		t.Fatal("a consumed approval ran again")
	}
	run("approval_deny", e.propose(t, e.alice, "update_lead_status", args), "")
	run("approval_bad_decision", e.propose(t, e.alice, "update_lead_status", args), "")
	run("approval_wrong_user", e.propose(t, e.bob, "update_lead_status", args), "")
	run("approval_unknown_id", "does-not-exist", "")
	run("approval_empty_id", "", "")
	run("approval_empty_list", "", "")
	if e.leadStatus(t, 4) != before {
		t.Fatal("a denied or invalid approval changed the lead")
	}

	// a missing decision defaults to approve (Python item.get("decision", "approve"))
	e.fake.script("approval_approve")
	id = e.propose(t, e.alice, "update_lead_status", args)
	rec := e.post("alice", "ws-alpha", `{"approved_tool_calls":[{"tool_call":{"id":"`+id+`"}}],"messages":[{"role":"user","content":"[sim:text_plain] go ahead"}]}`)
	if !strings.Contains(rec.Body.String(), `"tool_result": {"name": "update_lead_status"`) || e.leadStatus(t, 4) != "qualified" {
		t.Fatalf("default decision: %q", rec.Body.String())
	}
	// ResolveApproved ran before the llm round: the round carries the assistant tool_calls and the tool result
	last := e.fake.llmRequests()
	ms := last[len(last)-1]["messages"].([]any)
	if len(ms) < 4 || ms[len(ms)-2].(map[string]any)["role"] != "assistant" || ms[len(ms)-1].(map[string]any)["role"] != "tool" {
		t.Fatalf("llm messages %v", ms)
	}
}

func TestChatPreRouteHandledSkipsLLM(t *testing.T) {
	e := newChatEnv(t, chatOpts{})
	c := chatLoadContract(t, "pre_company_team")
	rec := e.post("alice", "ws-alpha", c.body)
	if chatNorm(rec.Body.String()) != chatNorm(c.expect) || len(e.fake.llmRequests()) != 0 {
		t.Fatalf("a handled pre-route must answer like Python and never reach the llm\n got %q\nwant %q",
			chatNorm(rec.Body.String()), chatNorm(c.expect))
	}
	conv := chatConvOf(t, rec.Body.String())
	if chatCount(t, `SELECT count(*) FROM chat_conversations WHERE id = $1 AND workspace_id = 'ws-alpha'`, conv) != 1 {
		t.Fatal("the branch must answer inside the opened conversation")
	}
}

func TestChatContextAssemblyOrder(t *testing.T) {
	e := newChatEnv(t, chatOpts{})
	td := `{"name": "search_leads", "result": {"leads": [{"id": 1}], "count": 1}}`
	_ = tenancy.InWorkspace(context.Background(), e.pool, "ws-alpha", func(tx pgx.Tx) error {
		if err := chathistory.AddMessage(context.Background(), tx, "conv-ed-1", "tool", "search_leads result", &td, "ws-alpha", &e.alice); err != nil {
			t.Fatal(err)
		}
		ns, _ := chatmemory.Namespace("ws-alpha", &e.alice)
		return chatmemory.Add(context.Background(), tx, ns, "User asked: acme pipeline\nAssistant answered about: acme leads", nil)
	})
	e.fake.script("chat_text_plain")
	rec := e.post("alice", "ws-alpha", `{"conversation_id":"conv-ed-1","context":"Viewing workbook wb-1","messages":[
		{"role":"user","content":"hello","junk":1},{"role":"assistant","content":"hi"},{"role":"function","name":"f","content":"old"},
		{"role":"tool","content":"orphan"},{"role":"user","content":"tell me about acme"}]}`)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	req := e.fake.llmRequests()[0]
	var got [][2]string
	for _, m := range req["messages"].([]any) {
		mm := m.(map[string]any)
		if len(mm) > 4 {
			t.Fatalf("unexpected keys %v", mm)
		}
		got = append(got, [2]string{mm["role"].(string), mm["content"].(string)})
	}
	// one system message (the Go-built prompt); stored tool results, memories and the client's context
	// share ONE user-role message between markers; client system/function/tool messages are dropped
	if len(got) != 5 || got[0][0] != "system" || !strings.Contains(got[0][1], "## Current Pipeline Stats") || got[1][0] != "user" ||
		!strings.HasPrefix(got[1][1], chatCtxIntro+chatCtxOpen+"\nUntrusted data returned by tools") ||
		!strings.HasSuffix(got[1][1], "\n"+chatCtxClose) ||
		!strings.Contains(got[1][1], `{"name":"search_leads"`) ||
		!strings.Contains(got[1][1], "Relevant memories from past conversations:\n- User asked: acme pipeline\nAssistant answered about: acme leads") ||
		!strings.Contains(got[1][1], "Current application context:\nViewing workbook wb-1") ||
		got[2] != [2]string{"user", "hello"} || got[3] != [2]string{"assistant", "hi"} ||
		got[4] != [2]string{"user", "tell me about acme"} {
		t.Fatalf("messages %q", got)
	}
}

func TestChatMidStreamFailureKeeps200WithErrorFrame(t *testing.T) {
	e := newChatEnv(t, chatOpts{})
	e.fake.llmFail = true
	rec := e.post("alice", "ws-alpha", `{"messages":[{"role":"user","content":"hi"}]}`)
	fs := chatFrames(rec.Body.String())
	if rec.Code != 200 || len(fs) != 3 || fs[1] != `data: {"error": "AI provider stream interrupted"}` || fs[2] != "data: [DONE]" {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), errChatFakeRound.Error()) {
		t.Fatal("provider error text in the stream")
	}
}

func TestChatStreamCap429(t *testing.T) {
	e := newChatEnv(t, chatOpts{perUser: 1})
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	e.fake.llmHold = make(chan struct{})
	tok := bearer("alice")["Authorization"]
	do := func(ctx context.Context) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/api/copilotkit", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", tok)
		req.Header.Set("X-Workspace-Id", "ws-alpha")
		return http.DefaultClient.Do(req)
	}
	first := make(chan string, 1)
	go func() {
		resp, err := do(context.Background())
		if err != nil {
			first <- err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		first <- string(b)
	}()
	select {
	case <-e.fake.llmStart:
	case <-time.After(5 * time.Second):
		t.Fatal("first stream never reached the llm")
	}
	before := e.convCount(t, "ws-alpha")
	resp, err := do(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 429 || strings.TrimSpace(string(b)) != `{"detail":"Too many active chat streams"}` {
		t.Fatalf("%d %q", resp.StatusCode, b)
	}
	if e.convCount(t, "ws-alpha") != before {
		t.Fatal("a refused stream created a conversation")
	}
	close(e.fake.llmHold)
	select {
	case body := <-first:
		if !strings.Contains(body, "[DONE]") {
			t.Fatalf("first stream: %q", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first stream did not finish")
	}
	chatWaitActive(t, e.caps, 0)
}

func chatWaitActive(t *testing.T, c *ssestream.Caps, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.Active() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Active() = %d, want %d", c.Active(), want)
}

func TestChatDisconnectReleasesSlotAndCancelsRound(t *testing.T) {
	e := newChatEnv(t, chatOpts{})
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	e.fake.llmHold = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/api/copilotkit", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", bearer("alice")["Authorization"])
	req.Header.Set("X-Workspace-Id", "ws-alpha")
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	select {
	case <-e.fake.llmStart:
	case <-time.After(5 * time.Second):
		t.Fatal("never reached the llm")
	}
	if e.caps.Active() != 1 {
		t.Fatalf("Active() = %d while streaming", e.caps.Active())
	}
	cancel()
	select {
	case <-e.fake.llmDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the llm round was not cancelled")
	}
	chatWaitActive(t, e.caps, 0)
	time.Sleep(100 * time.Millisecond)
	// Python stores nothing after a disconnect: no assistant row, no memory
	if n := chatCount(t, `SELECT count(*) FROM chat_messages WHERE workspace_id IN ('ws-alpha','ws-beta') AND role = 'assistant'`); n != 0 {
		t.Fatalf("%d assistant rows after a disconnect", n)
	}
}

func TestChatPanicReleasesSlot(t *testing.T) {
	e := newChatEnv(t, chatOpts{configure: func(tn *copilot.Turn) {
		tn.Rounds = func(context.Context, copilot.RoundRequest) (copilot.FrameSource, error) { panic("boom") }
	}})
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the handler did not panic")
			}
		}()
		req := httptest.NewRequest("POST", "/api/copilotkit", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", bearer("alice")["Authorization"])
		req.Header.Set("X-Workspace-Id", "ws-alpha")
		e.hnd.ServeHTTP(httptest.NewRecorder(), req)
	}()
	if e.caps.Active() != 0 {
		t.Fatalf("Active() = %d after a panic", e.caps.Active())
	}
}

func TestChatNoSecretInLogs(t *testing.T) {
	e := newChatEnv(t, chatOpts{})
	const content = "SECRET-MESSAGE-BODY-77"
	tok := bearer("alice")["Authorization"]
	e.fake.llmFail = true
	e.post("alice", "ws-alpha", `{"context":"CTX-SECRET-5","messages":[{"role":"user","content":"`+content+`"}]}`)
	e.post("alice", "ws-alpha", `{"messages":"`+content+`"}`)
	logs := e.logs.String()
	if logs == "" {
		t.Fatal("expected the route to log its failures")
	}
	for _, s := range []string{content, "CTX-SECRET-5", strings.TrimPrefix(tok, "Bearer ")} {
		if strings.Contains(logs, s) {
			t.Fatalf("log leaks %q:\n%s", s, logs)
		}
	}
}

func chatCount(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := wcrudOwnerQueryRow(t, q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The turn deadline covers the whole turn: a hung llm round ends by the deadline with the timeout frame and frees
// the slot (the conversation_id frame has started the response by then).
func TestChatTurnDeadlineCoversHungRound(t *testing.T) {
	e := newChatEnv(t, chatOpts{})
	e.hnd.turnTimeout = 600 * time.Millisecond
	e.fake.llmHold = make(chan struct{})
	defer close(e.fake.llmHold)
	res := make(chan *httptest.ResponseRecorder, 1)
	go func() { res <- e.post("alice", "ws-alpha", `{"messages":[{"role":"user","content":"hi"}]}`) }()
	select {
	case rec := <-res:
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `{"error": "Request timed out"}`) || !strings.HasSuffix(rec.Body.String(), "data: [DONE]\n\n") {
			t.Fatalf("%d %q", rec.Code, rec.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a hung round outlived the turn deadline")
	}
	chatWaitActive(t, e.caps, 0)
}
