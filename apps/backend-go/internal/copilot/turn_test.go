package copilot

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
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/copilotauth"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
)

// ---- helpers shared by turn_test, round_test, gate_test ----

// syncRec is a goroutine-safe flushing ResponseWriter (the keepalive and the producer write concurrently).
type syncRec struct {
	mu  sync.Mutex
	buf bytes.Buffer
	h   http.Header
}

func newRec() *syncRec                 { return &syncRec{h: http.Header{}} }
func (r *syncRec) Header() http.Header { return r.h }
func (r *syncRec) WriteHeader(int)     {}
func (r *syncRec) Flush()              {}
func (r *syncRec) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(b)
}
func (r *syncRec) String() string { r.mu.Lock(); defer r.mu.Unlock(); return r.buf.String() }

type toolReq struct {
	Header http.Header
	Body   map[string]any
}

// fakePy is the Python worker: scripted llm rounds, tool and describe endpoints.
type fakePy struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	rounds   [][]string // NDJSON lines per llm call
	llmReqs  []map[string]any
	tools    []toolReq
	describe []map[string]any
	// optional behaviours
	llmHold  func(w http.ResponseWriter, r *http.Request, n int) bool // return true when handled
	toolFn   func(req toolReq) (int, any)
	descFn   func(body map[string]any) (int, any)
	llmDone  chan struct{} // closed when an llm request's context ends
	llmStart chan struct{}
}

func newFakePy(t *testing.T, rounds ...[]string) *fakePy {
	f := &fakePy{t: t, rounds: rounds, llmDone: make(chan struct{}, 8), llmStart: make(chan struct{}, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/copilot/llm", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		n := len(f.llmReqs)
		f.llmReqs = append(f.llmReqs, body)
		f.mu.Unlock()
		f.llmStart <- struct{}{}
		if f.llmHold != nil && f.llmHold(w, r, n) {
			<-r.Context().Done()
			f.llmDone <- struct{}{}
			return
		}
		if n >= len(f.rounds) {
			http.Error(w, "no script", 500)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, l := range f.rounds[n] {
			var cb bytes.Buffer
			if json.Compact(&cb, []byte(l)) == nil {
				l = cb.String()
			}
			_, _ = io.WriteString(w, l+"\n")
			w.(http.Flusher).Flush()
		}
	})
	mux.HandleFunc("/internal/copilot/tool", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		req := toolReq{Header: r.Header.Clone(), Body: body}
		f.mu.Lock()
		f.tools = append(f.tools, req)
		f.mu.Unlock()
		st, out := 200, any(map[string]any{"result": `{"ok": true}`})
		if f.toolFn != nil {
			st, out = f.toolFn(req)
		}
		w.WriteHeader(st)
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/internal/copilot/describe", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.describe = append(f.describe, body)
		f.mu.Unlock()
		st, out := 200, any(map[string]any{"description": "DESC " + fmt.Sprint(body["name"])})
		if f.descFn != nil {
			st, out = f.descFn(body)
		}
		w.WriteHeader(st)
		_ = json.NewEncoder(w).Encode(out)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePy) client() *pyworker.Client { return pyworker.New(f.srv.URL, "tok-secret-9f3", nil) }
func (f *fakePy) llmCount() int            { f.mu.Lock(); defer f.mu.Unlock(); return len(f.llmReqs) }
func (f *fakePy) toolCount() int           { f.mu.Lock(); defer f.mu.Unlock(); return len(f.tools) }
func (f *fakePy) llmReq(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.llmReqs[i]
}

type settingsStub map[string]string

func (s settingsStub) Get(_ context.Context, key, def string) (string, error) {
	if v, ok := s[key]; ok {
		return v, nil
	}
	return def, nil
}

func uidp(n int32) *int32 { return &n }

func newTurn(f *fakePy, native NativeFunc) *Turn {
	d := Deps{PY: f.client(), Native: native, Settings: settingsStub{}, ToolTimeout: 5 * time.Second, TurnTimeout: 30 * time.Second}
	return &Turn{Deps: d, WS: "ws-t13", Slug: "alpha", UID: uidp(7), ConvID: "c1",
		Auth: copilotauth.Resolved{WorkspaceID: "ws-t13", Slug: "alpha", UserID: uidp(7)}}
}

func userMsgs() []map[string]any {
	return []map[string]any{{"role": "system", "content": "SYSTEM"}, {"role": "user", "content": "hi"}}
}

func runTurn(t *testing.T, tn *Turn, msgs []map[string]any) (string, error) {
	t.Helper()
	rec := newRec()
	w, err := ssestream.New(rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = tn.Run(context.Background(), w, msgs)
	return rec.String(), err
}

func frame(s string) string { return "data: " + s + "\n\n" }

const done = "data: [DONE]\n\n"

func nativeConst(res string) NativeFunc {
	return func(context.Context, string, map[string]any, string, *int32) (string, bool, error) {
		return res, true, nil
	}
}

// ---- tests ----

func TestPlainAnswerStreamsContent(t *testing.T) {
	f := newFakePy(t, []string{`{"t":"content","d":"Hi"}`, `{"t":"content","d":" thére"}`, `{"t":"done","cursor":0}`})
	body, err := runTurn(t, newTurn(f, nil), userMsgs())
	if err != nil {
		t.Fatal(err)
	}
	want := frame(`{"content": "Hi"}`) + frame(`{"content": " th\u00e9re"}`) + done
	if body != want {
		t.Fatalf("got %q want %q", body, want)
	}
	req := f.llmReq(0)
	if req["workspace_id"] != "ws-t13" || req["tools_enabled"] != true || req["round_budget_hit"] != false || req["provider_cursor"] != float64(0) {
		t.Fatalf("request %v", req)
	}
	if m := req["messages"].([]any); len(m) != 2 {
		t.Fatalf("messages %v", m)
	}
}

func TestFailoverWarningPassesThrough(t *testing.T) {
	f := newFakePy(t, []string{`{"t":"warning","d":"Provider A rate limited"}`, `{"t":"content","d":"\n\n> switching"}`, `{"t":"done","cursor":1}`})
	body, _ := runTurn(t, newTurn(f, nil), userMsgs())
	want := frame(`{"warning": "Provider A rate limited"}`) + frame(`{"content": "\n\n> switching"}`) + done
	if body != want {
		t.Fatalf("got %q", body)
	}
}

func TestWorkerErrorFrameIsTerminalWithoutDone(t *testing.T) {
	f := newFakePy(t, []string{`{"t":"warning","d":"w"}`, `{"t":"error","d":"Cannot reach AI provider."}`})
	body, err := runTurn(t, newTurn(f, nil), userMsgs())
	if err != nil {
		t.Fatal(err)
	}
	if want := frame(`{"warning": "w"}`) + frame(`{"error": "Cannot reach AI provider."}`); body != want {
		t.Fatalf("got %q", body)
	}
}

func TestProviderDoneFalseSendsNoDone(t *testing.T) {
	f := newFakePy(t, []string{`{"t":"content","d":"x"}`, `{"t":"done","cursor":0,"provider_done":false}`})
	body, _ := runTurn(t, newTurn(f, nil), userMsgs())
	if want := frame(`{"content": "x"}`); body != want {
		t.Fatalf("got %q", body)
	}
}

func TestSafeToolCallRoundTrip(t *testing.T) {
	calls := `[{"id":"c1","type":"function","function":{"name":"search_leads","arguments":"{\"query\": \"acme\", \"limit\": 3}"}}]`
	f := newFakePy(t,
		[]string{`{"t":"tool_calls","d":` + calls + `,"cursor":2}`},
		[]string{`{"t":"content","d":"Found."}`, `{"t":"done","cursor":2}`})
	var gotArgs map[string]any
	tn := newTurn(f, func(_ context.Context, name string, args map[string]any, ws string, uid *int32) (string, bool, error) {
		gotArgs = args
		if name != "search_leads" || ws != "ws-t13" || uid == nil || *uid != 7 {
			t.Errorf("native called with %s %s %v", name, ws, uid)
		}
		return `{"leads": [{"id": 1, "x": "é"}], "count": 1}`, true, nil
	})
	body, err := runTurn(t, tn, userMsgs())
	if err != nil {
		t.Fatal(err)
	}
	want := frame(`{"tool_call": {"name": "search_leads", "args": {"query": "acme", "limit": 3}}}`) +
		frame(`{"tool_result": {"name": "search_leads", "result": {"leads": [{"id": 1, "x": "\u00e9"}], "count": 1}}}`) +
		frame(`{"content": "Found."}`) + done
	if body != want {
		t.Fatalf("got %q\nwant %q", body, want)
	}
	if gotArgs["query"] != "acme" {
		t.Fatalf("args %v", gotArgs)
	}
	r2 := f.llmReq(1)
	if r2["provider_cursor"] != float64(2) {
		t.Fatalf("cursor not forwarded: %v", r2["provider_cursor"])
	}
	m := r2["messages"].([]any)
	if len(m) != 4 {
		t.Fatalf("messages %v", m)
	}
	as := m[2].(map[string]any)
	if as["role"] != "assistant" || as["content"] != "" || len(as["tool_calls"].([]any)) != 1 {
		t.Fatalf("assistant %v", as)
	}
	tm := m[3].(map[string]any)
	if tm["role"] != "tool" || tm["tool_call_id"] != "c1" || tm["name"] != "search_leads" || tm["content"] != `{"leads": [{"id": 1, "x": "é"}], "count": 1}` {
		t.Fatalf("tool message %v", tm)
	}
}

func TestDuplicateSafeCallReusesResult(t *testing.T) {
	calls := `[{"id":"a","type":"function","function":{"name":"search_leads","arguments":"{\"a\": 1, \"b\": 2}"}},
	 {"id":"b","type":"function","function":{"name":"search_leads","arguments":"{\"b\": 2, \"a\": 1}"}}]`
	f := newFakePy(t, []string{`{"t":"tool_calls","d":` + calls + `,"cursor":0}`}, []string{`{"t":"done","cursor":0}`})
	var n atomic.Int32
	tn := newTurn(f, func(context.Context, string, map[string]any, string, *int32) (string, bool, error) {
		n.Add(1)
		return `{"n": 1}`, true, nil
	})
	body, _ := runTurn(t, tn, userMsgs())
	if n.Load() != 1 {
		t.Fatalf("executed %d times", n.Load())
	}
	want := frame(`{"tool_call": {"name": "search_leads", "args": {"a": 1, "b": 2}}}`) + frame(`{"tool_result": {"name": "search_leads", "result": {"n": 1}}}`) +
		frame(`{"tool_call": {"name": "search_leads", "args": {"b": 2, "a": 1}}}`) + frame(`{"tool_result": {"name": "search_leads", "result": {"n": 1}}}`) + done
	if body != want {
		t.Fatalf("got %q", body)
	}
	m := f.llmReq(1)["messages"].([]any)
	if len(m) != 5 || m[3].(map[string]any)["tool_call_id"] != "a" || m[4].(map[string]any)["tool_call_id"] != "b" {
		t.Fatalf("messages %v", m)
	}
}

func TestParallelSafeCallsOrder(t *testing.T) {
	calls := `[{"id":"a","type":"function","function":{"name":"get_lead_stats","arguments":""}},
	 {"id":"b","type":"function","function":{"name":"search_leads","arguments":"not json"}},
	 {"id":"c","type":"function","function":{"name":"get_lead_detail","arguments":"{\"lead_id\": 5}"}}]`
	f := newFakePy(t, []string{`{"t":"tool_calls","d":` + calls + `,"cursor":0}`}, []string{`{"t":"done","cursor":0}`})
	var order []string
	tn := newTurn(f, func(_ context.Context, name string, _ map[string]any, _ string, _ *int32) (string, bool, error) {
		order = append(order, name)
		return `{"r": "` + name + `"}`, true, nil
	})
	body, _ := runTurn(t, tn, userMsgs())
	if strings.Join(order, ",") != "get_lead_stats,search_leads,get_lead_detail" {
		t.Fatalf("order %v", order)
	}
	want := frame(`{"tool_call": {"name": "get_lead_stats", "args": {}}}`) + frame(`{"tool_result": {"name": "get_lead_stats", "result": {"r": "get_lead_stats"}}}`) +
		frame(`{"tool_call": {"name": "search_leads", "args": {}}}`) + frame(`{"tool_result": {"name": "search_leads", "result": {"r": "search_leads"}}}`) +
		frame(`{"tool_call": {"name": "get_lead_detail", "args": {"lead_id": 5}}}`) + frame(`{"tool_result": {"name": "get_lead_detail", "result": {"r": "get_lead_detail"}}}`) + done
	if body != want {
		t.Fatalf("got %q", body)
	}
	// "" arguments are sent back to the provider as "{}"
	tc := f.llmReq(1)["messages"].([]any)[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if tc["arguments"] != "{}" {
		t.Fatalf("arguments %v", tc["arguments"])
	}
}

func toolRound(name string, i int) string {
	return fmt.Sprintf(`{"t":"tool_calls","d":[{"id":"c%d","type":"function","function":{"name":%q,"arguments":"{\"q\": %d}"}}],"cursor":0}`, i, name, i)
}

func TestRoundBudgetDisablesTools(t *testing.T) {
	f := newFakePy(t, []string{toolRound("search_leads", 0)}, []string{toolRound("search_leads", 1)}, []string{`{"t":"content","d":"summary"}`, `{"t":"done","cursor":0}`})
	tn := newTurn(f, nativeConst(`{"count": 0}`))
	tn.Settings = settingsStub{"CHAT_MAX_TOOL_ROUNDS": "2"}
	body, _ := runTurn(t, tn, userMsgs())
	if !strings.HasSuffix(body, frame(`{"content": "summary"}`)+done) {
		t.Fatalf("got %q", body)
	}
	for i := 0; i < 2; i++ {
		if r := f.llmReq(i); r["tools_enabled"] != true || r["round_budget_hit"] != false {
			t.Fatalf("round %d: %v", i, r)
		}
	}
	r := f.llmReq(2)
	if r["tools_enabled"] != false || r["round_budget_hit"] != true {
		t.Fatalf("final round flags: %v", r)
	}
	ms := r["messages"].([]any)
	last := ms[len(ms)-1].(map[string]any)
	want := "You have reached the maximum of 2 tool-use rounds. Do NOT call any more tools. Summarize what you found and the next step the user can take."
	if last["role"] != "system" || last["content"] != want {
		t.Fatalf("budget message %v", last)
	}
}

func TestMaxRoundsFloorAndInvalid(t *testing.T) {
	for _, tc := range []struct{ val, want string }{{"0", "1"}, {"-4", "1"}, {"abc", "8"}} {
		f := newFakePy(t, []string{toolRound("search_leads", 0)}, []string{`{"t":"done","cursor":0}`})
		tn := newTurn(f, nativeConst(`{}`))
		tn.Settings = settingsStub{"CHAT_MAX_TOOL_ROUNDS": tc.val}
		if _, err := runTurn(t, tn, userMsgs()); err != nil {
			t.Fatal(err)
		}
		r := f.llmReq(1)
		if tc.want == "1" && r["round_budget_hit"] != true {
			t.Fatalf("%s: budget not hit after round 1: %v", tc.val, r)
		}
		if tc.want == "8" && r["round_budget_hit"] != false {
			t.Fatalf("%s: budget hit early: %v", tc.val, r)
		}
	}
}

func TestPythonToolUsesToolUserAndCallID(t *testing.T) {
	calls := `[{"id":"p1","type":"function","function":{"name":"find_similar_leads","arguments":"{\"lead_id\": 3}"}}]`
	f := newFakePy(t, []string{`{"t":"tool_calls","d":` + calls + `,"cursor":0}`}, []string{`{"t":"done","cursor":0}`})
	f.toolFn = func(toolReq) (int, any) { return 200, map[string]any{"result": `{"similar": []}`} }
	body, err := runTurn(t, newTurn(f, nil), userMsgs())
	if err != nil {
		t.Fatal(err)
	}
	if f.toolCount() != 1 {
		t.Fatalf("tool calls %d", f.toolCount())
	}
	req := f.tools[0]
	b := req.Body
	if b["name"] != "find_similar_leads" || b["workspace_id"] != "ws-t13" || b["user_id"] != float64(7) || b["slug"] != "alpha" || b["timeout_s"] != float64(5) {
		t.Fatalf("tool body %v", b)
	}
	if req.Header.Get("X-Call-Id") == "" || !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
		t.Fatalf("headers %v", req.Header)
	}
	if !strings.Contains(body, `{"tool_result": {"name": "find_similar_leads", "result": {"similar": []}}}`) {
		t.Fatalf("body %q", body)
	}
}

func TestPythonToolWithoutToolUserFailsTurn(t *testing.T) {
	calls := `[{"id":"p1","type":"function","function":{"name":"find_similar_leads","arguments":"{}"}}]`
	f := newFakePy(t, []string{`{"t":"tool_calls","d":` + calls + `,"cursor":0}`}, []string{`{"t":"done","cursor":0}`})
	tn := newTurn(f, nil)
	tn.Auth = copilotauth.Resolved{WorkspaceID: "ws-t13"} // no user, not self-host: fails closed
	body, err := runTurn(t, tn, userMsgs())
	if err == nil {
		t.Fatal("want an error")
	}
	if f.toolCount() != 0 || f.llmCount() != 1 {
		t.Fatalf("tool %d llm %d", f.toolCount(), f.llmCount())
	}
	if !strings.HasSuffix(body, done) || !strings.Contains(body, `"error"`) {
		t.Fatalf("body %q", body)
	}
}

func TestToolTimeoutBecomesToolErrorResult(t *testing.T) {
	calls := `[{"id":"p1","type":"function","function":{"name":"find_similar_leads","arguments":"{}"}}]`
	f := newFakePy(t, []string{`{"t":"tool_calls","d":` + calls + `,"cursor":0}`}, []string{`{"t":"content","d":"sorry"}`, `{"t":"done","cursor":0}`})
	f.toolFn = func(toolReq) (int, any) { return 408, map[string]any{"detail": "timeout"} }
	body, err := runTurn(t, newTurn(f, nil), userMsgs())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, frame(`{"tool_result": {"name": "find_similar_leads", "result": {"error": "Tool 'find_similar_leads' failed: timed out"}}}`)) {
		t.Fatalf("body %q", body)
	}
	if f.toolCount() != 1 {
		t.Fatalf("a timed-out tool must not be retried: %d", f.toolCount())
	}
	m := f.llmReq(1)["messages"].([]any)
	if c := m[3].(map[string]any)["content"]; c != `{"error": "Tool 'find_similar_leads' failed: timed out"}` {
		t.Fatalf("content %v", c)
	}
}

func TestNativeDatabaseErrorMapsToToolFailed(t *testing.T) {
	f := newFakePy(t, []string{toolRound("search_leads", 0)}, []string{`{"t":"done","cursor":0}`})
	tn := newTurn(f, func(context.Context, string, map[string]any, string, *int32) (string, bool, error) {
		return "", false, errors.New(`ERROR: relation "secret_table" does not exist (SQLSTATE 42P01)`)
	})
	body, _ := runTurn(t, tn, userMsgs())
	if strings.Contains(body, "secret_table") || !strings.Contains(body, `"error": "Tool 'search_leads' failed`) {
		t.Fatalf("body %q", body)
	}
}

func TestUnknownPythonToolAnswers404AsUnknownTool(t *testing.T) {
	calls := `[{"id":"p1","type":"function","function":{"name":"find_similar_leads","arguments":"{}"}}]`
	f := newFakePy(t, []string{`{"t":"tool_calls","d":` + calls + `,"cursor":0}`}, []string{`{"t":"done","cursor":0}`})
	f.toolFn = func(toolReq) (int, any) { return 404, map[string]any{"detail": "unknown tool"} }
	body, _ := runTurn(t, newTurn(f, nil), userMsgs())
	if !strings.Contains(body, `"result": {"error": "Unknown tool: find_similar_leads"}`) {
		t.Fatalf("body %q", body)
	}
}

func TestNonJSONToolOutputBecomesRaw(t *testing.T) {
	f := newFakePy(t, []string{toolRound("search_leads", 0)}, []string{`{"t":"done","cursor":0}`})
	body, _ := runTurn(t, newTurn(f, nativeConst("plain text é")), userMsgs())
	if !strings.Contains(body, `{"tool_result": {"name": "search_leads", "result": {"raw": "plain text \u00e9"}}}`) {
		t.Fatalf("body %q", body)
	}
	if !strings.HasSuffix(body, done) {
		t.Fatalf("turn must continue: %q", body)
	}
}

func TestOversizedToolResultIsReplaced(t *testing.T) {
	f := newFakePy(t, []string{toolRound("search_leads", 0)}, []string{`{"t":"done","cursor":0}`})
	big := `"` + strings.Repeat("a", 1<<20+10) + `"`
	body, _ := runTurn(t, newTurn(f, nativeConst(big)), userMsgs())
	if !strings.Contains(body, `"result": {"error": "tool result too large"}`) {
		t.Fatalf("body len %d", len(body))
	}
}

func TestPythonCrashMidStreamEmitsErrorThenDone(t *testing.T) {
	// content, then the connection ends with no terminal frame
	f := newFakePy(t, []string{`{"t":"content","d":"par"}`})
	body, _ := runTurn(t, newTurn(f, nil), userMsgs())
	want := frame(`{"content": "par"}`) + frame(`{"error": "AI provider stream interrupted"}`) + done
	if body != want {
		t.Fatalf("got %q", body)
	}
}

func TestProtocolErrorEmitsErrorThenDone(t *testing.T) {
	f := newFakePy(t, []string{`not json at all`})
	body, _ := runTurn(t, newTurn(f, nil), userMsgs())
	if want := frame(`{"error": "AI provider stream interrupted"}`) + done; body != want {
		t.Fatalf("got %q", body)
	}
}

func TestWorkerHTTPFailureNeverLeaksSecrets(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)
	f := newFakePy(t)
	f.llmHold = nil
	f.rounds = nil // 500 "no script"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "sk-live-SECRETKEY "+r.Header.Get("Authorization"), 500)
	}))
	defer srv.Close()
	tn := newTurn(f, nil)
	tn.PY = pyworker.New(srv.URL, "tok-secret-9f3", nil)
	body, err := runTurn(t, tn, userMsgs())
	all := body + fmt.Sprint(err) + logs.String()
	for _, s := range []string{"SECRETKEY", "tok-secret-9f3"} {
		if strings.Contains(all, s) {
			t.Fatalf("secret %q leaked in %q", s, all)
		}
	}
	if want := frame(`{"error": "AI provider stream interrupted"}`) + done; body != want {
		t.Fatalf("got %q", body)
	}
}

func TestClientDisconnectCancelsRPC(t *testing.T) {
	f := newFakePy(t)
	f.llmHold = func(w http.ResponseWriter, r *http.Request, n int) bool {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, `{"t":"content","d":"x"}`+"\n")
		w.(http.Flusher).Flush()
		return true
	}
	rec := newRec()
	w, _ := ssestream.New(rec, nil)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- newTurn(f, nil).Run(ctx, w, userMsgs()) }()
	<-f.llmStart
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	select {
	case <-f.llmDone:
	case <-time.After(3 * time.Second):
		t.Fatal("python side was not cancelled")
	}
	if strings.Contains(rec.String(), "[DONE]") {
		t.Fatalf("no terminal frame for a disconnected client: %q", rec.String())
	}
}

func TestTurnDeadline(t *testing.T) {
	f := newFakePy(t)
	f.llmHold = func(w http.ResponseWriter, r *http.Request, n int) bool { return true }
	tn := newTurn(f, nil)
	tn.TurnTimeout = 150 * time.Millisecond
	start := time.Now()
	body, err := runTurn(t, tn, userMsgs())
	if time.Since(start) > 3*time.Second {
		t.Fatal("deadline not enforced")
	}
	if want := frame(`{"error": "Request timed out"}`) + done; body != want {
		t.Fatalf("got %q (err %v)", body, err)
	}
	select {
	case <-f.llmDone:
	case <-time.After(3 * time.Second):
		t.Fatal("python side was not cancelled at the deadline")
	}
}

func TestKeepaliveDuringSlowTool(t *testing.T) {
	f := newFakePy(t, []string{toolRound("search_leads", 0)}, []string{`{"t":"done","cursor":0}`})
	release := make(chan struct{})
	started := make(chan struct{})
	tick := make(chan time.Time)
	tn := newTurn(f, func(context.Context, string, map[string]any, string, *int32) (string, bool, error) {
		close(started)
		<-release
		return `{"slow": true}`, true, nil
	})
	var gotInterval time.Duration
	tn.NewTicker = func(d time.Duration) (<-chan time.Time, func()) { gotInterval = d; return tick, func() {} }
	rec := newRec()
	w, _ := ssestream.New(rec, nil)
	errc := make(chan error, 1)
	go func() { errc <- tn.Run(context.Background(), w, userMsgs()) }()
	<-started
	tick <- time.Now()
	tick <- time.Now()
	tick <- time.Now() // unbuffered: the third send proves the first two were consumed
	close(release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if gotInterval != 10*time.Second {
		t.Fatalf("interval %v", gotInterval)
	}
	body := rec.String()
	if n := strings.Count(body, ": keepalive\n\n"); n < 2 {
		t.Fatalf("keepalives %d in %q", n, body)
	}
	if i, j := strings.Index(body, ": keepalive"), strings.Index(body, `{"tool_result"`); i < 0 || j < i {
		t.Fatalf("keepalive must precede the result: %q", body)
	}
}

func TestSlotReleasedAfterPanicAndCancel(t *testing.T) {
	caps := ssestream.NewCaps(4, 2)
	run := func(ctx context.Context, f func(*Turn)) {
		rel, err := caps.Acquire(ctx, "ws-t13", 7)
		if err != nil {
			t.Fatal(err)
		}
		defer rel()
		fp := newFakePy(t, []string{`{"t":"content","d":"x"}`, `{"t":"done","cursor":0}`})
		tn := newTurn(fp, nil)
		f(tn)
		rec := newRec()
		w, _ := ssestream.New(rec, nil)
		_ = tn.Run(ctx, w, userMsgs())
	}
	func() {
		defer func() { _ = recover() }()
		run(context.Background(), func(tn *Turn) { tn.OnEvent = func([]byte) { panic("boom") } })
	}()
	if caps.Active() != 0 {
		t.Fatalf("active after panic: %d", caps.Active())
	}
	ctx, cancel := context.WithCancel(context.Background())
	run(ctx, func(*Turn) { cancel() })
	if caps.Active() != 0 {
		t.Fatalf("active after cancel: %d", caps.Active())
	}
}

func TestOnEventSeesEveryDataFrame(t *testing.T) {
	f := newFakePy(t, []string{`{"t":"content","d":"a"}`, `{"t":"done","cursor":0}`})
	tn := newTurn(f, nil)
	var got []string
	tn.OnEvent = func(raw []byte) { got = append(got, string(raw)) }
	_, _ = runTurn(t, tn, userMsgs())
	if len(got) != 1 || got[0] != `{"content": "a"}` {
		t.Fatalf("events %q", got)
	}
}

// ---- replay of the recorded scenarios ----

var hex32 = regexp.MustCompile(`[0-9a-f]{32}`)

func norm(s string) string { return hex32.ReplaceAllString(s, "<id>") }

type recordedRPC struct {
	RPC    string            `json:"rpc"`
	Frames []json.RawMessage `json:"frames"`
}

func scenarioNames(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("../../testdata/copilot/frames/*chat_*.ndjson")
	if err != nil || len(files) == 0 {
		t.Fatalf("no frame fixtures: %v", err)
	}
	var out []string
	for _, f := range files {
		out = append(out, strings.TrimSuffix(filepath.Base(f), ".ndjson"))
	}
	return out
}

// replayScenario drives Turn with the recorded llm rounds and returns (actual, expected) SSE bodies, the
// expected body being the legacy FastAPI bytes minus the route-owned conversation_id frame.
func replayScenario(t *testing.T, name string, pool *pgxpool.Pool) (actual, expected string, dangerous bool) {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/copilot/frames/" + name + ".ndjson")
	if err != nil {
		t.Fatal(err)
	}
	var rounds [][]string
	var first []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r struct {
			RPC     string `json:"rpc"`
			Request struct {
				Messages []map[string]any `json:"messages"`
			} `json:"request"`
			Frames []json.RawMessage `json:"frames"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r.RPC != "llm" {
			continue
		}
		if first == nil {
			first = r.Request.Messages
		}
		var ls []string
		for _, fr := range r.Frames {
			ls = append(ls, string(fr))
		}
		rounds = append(rounds, ls)
	}
	cname := strings.TrimPrefix(name, "selfhost_")
	cf := "../../testdata/contract/copilot/" + name + ".json"
	if _, err := os.Stat(cf); err != nil {
		cf = "../../testdata/contract/copilot/" + cname + ".json"
	}
	craw, err := os.ReadFile(cf)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Response struct {
			Body string `json:"body"`
		} `json:"response"`
	}
	if err := json.Unmarshal(craw, &c); err != nil {
		t.Fatal(err)
	}
	body := c.Response.Body
	i := strings.Index(body, "\n\n")
	if !strings.HasPrefix(body, `data: {"conversation_id"`) || i < 0 {
		t.Fatalf("%s: fixture lacks conversation_id frame", name)
	}
	expected = body[i+2:]
	dangerous = strings.Contains(expected, "confirmation_required")

	// tool results and descriptions come from the expected body (the recorder ran the real tools)
	results := map[string]string{}
	descs := map[string]string{}
	var lastSig string
	for _, ev := range strings.Split(strings.TrimSpace(expected), "\n\n") {
		if !strings.HasPrefix(ev, "data: {") {
			continue
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal([]byte(ev[6:]), &m)
		switch {
		case m["tool_call"] != nil:
			var tc struct {
				Name string         `json:"name"`
				Args map[string]any `json:"args"`
			}
			_ = json.Unmarshal(m["tool_call"], &tc)
			lastSig = tc.Name + "|" + sortedJSON(tc.Args)
		case m["tool_result"] != nil:
			var tr struct {
				Result json.RawMessage `json:"result"`
			}
			_ = json.Unmarshal(m["tool_result"], &tr)
			if _, ok := results[lastSig]; !ok {
				results[lastSig] = string(tr.Result)
			}
		case m["confirmation_required"] != nil:
			var cr struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			_ = json.Unmarshal(m["confirmation_required"], &cr)
			descs[cr.Name] = cr.Description
		}
	}
	f := newFakePy(t)
	f.rounds = rounds
	f.descFn = func(b map[string]any) (int, any) {
		return 200, map[string]any{"description": descs[fmt.Sprint(b["name"])]}
	}
	tn := newTurn(f, func(_ context.Context, name string, args map[string]any, _ string, _ *int32) (string, bool, error) {
		r, ok := results[name+"|"+sortedJSON(args)]
		if !ok {
			t.Errorf("%s: unexpected native call %s %v", name, name, args)
		}
		return r, true, nil
	})
	tn.Pool = pool
	if strings.Contains(name, "tool_loop_budget") {
		tn.Settings = settingsStub{"CHAT_MAX_TOOL_ROUNDS": "3"}
	}
	rec := newRec()
	w, _ := ssestream.New(rec, nil)
	if err := tn.Run(context.Background(), w, first); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if f.toolCount() != 0 && dangerous {
		t.Fatalf("%s: a dangerous call reached the python tool endpoint", name)
	}
	return rec.String(), expected, dangerous
}

func sortedJSON(m map[string]any) string {
	b, _ := json.Marshal(m) // encoding/json sorts map keys
	return string(b)
}

func TestFramesReplayEqualsLegacyBytes(t *testing.T) {
	for _, name := range scenarioNames(t) {
		t.Run(name, func(t *testing.T) {
			if strings.Contains(name, "dangerous") || strings.Contains(name, "unknown") {
				t.Skip("needs the approval table: covered by TestFramesReplayDangerousEqualsLegacyBytes")
			}
			got, want, dangerous := replayScenarioNoDB(t, name)
			if dangerous {
				t.Skip("needs the approval table")
			}
			if norm(got) != norm(want) {
				t.Fatalf("bytes differ\n got %q\nwant %q", got, want)
			}
		})
	}
}

func replayScenarioNoDB(t *testing.T, name string) (string, string, bool) {
	t.Helper()
	return replayScenario(t, name, nil)
}

// An approved dangerous tool must never be re-sent after the request may have reached Python (the
// worker's X-Call-Id dedupe is per process, so a retry on a fresh process would run it twice).
func TestDangerousToolRPCIsNotRetried(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close() // connection drops after the request arrived
	}))
	defer srv.Close()
	tn := newTurn(newFakePy(t), nil)
	tn.PY = pyworker.New(srv.URL, "tok", nil)
	res, err := tn.pythonTool(context.Background(), parsedCall{Name: "create_people_workbook", Args: map[string]any{}})
	if err != nil || !strings.Contains(res, "worker error") {
		t.Fatalf("res %q err %v", res, err)
	}
	if n.Load() != 1 {
		t.Fatalf("dangerous tool RPC sent %d times", n.Load())
	}
}

// A model-steered flood of dangerous calls in one frame ends the turn with a fixed error before any
// describe RPC or approval row (Python has no cap; Go caps at MaxApproved per round).
func TestTooManyDangerousCallsEndsTurn(t *testing.T) {
	var cs []string
	for i := 0; i <= MaxApproved; i++ {
		cs = append(cs, fmt.Sprintf(`{"id":"d%d","type":"function","function":{"name":"create_people_workbook","arguments":"{}"}}`, i))
	}
	f := newFakePy(t, []string{`{"t":"tool_calls","d":[` + strings.Join(cs, ",") + `],"cursor":0}`})
	body, err := runTurn(t, newTurn(f, nil), userMsgs())
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(body, `"error": "Too many tool calls requested"`) || !strings.HasSuffix(body, done) || strings.Contains(body, "confirmation_required") {
		t.Fatalf("body %q", body)
	}
}
