package copilot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
)

// fakeAnthropic answers POST /v1/messages from a script and records the bodies.
type fakeAnthropic struct {
	mu      sync.Mutex
	replies []struct {
		status int
		body   string
	}
	bodies []map[string]any
	raw    []string
}

func (f *fakeAnthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	f.mu.Lock()
	f.bodies = append(f.bodies, m)
	f.raw = append(f.raw, string(b))
	rep := f.replies[min(len(f.bodies)-1, len(f.replies)-1)]
	f.mu.Unlock()
	if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "sk-ant-test-key" {
		w.WriteHeader(404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-should-retry", "false")
	w.WriteHeader(rep.status)
	_, _ = w.Write([]byte(rep.body))
}

func (f *fakeAnthropic) reply(status int, body string) *fakeAnthropic {
	f.replies = append(f.replies, struct {
		status int
		body   string
	}{status, body})
	return f
}

func anthropicTurnFor(srvURL string, fallbacks ...llm.ChatProvider) *Turn {
	chain := append([]llm.ChatProvider{{ID: "anthropic", Name: "Anthropic", APIKey: "sk-ant-test-key", BaseURL: srvURL, Model: "claude-opus-4-8"}}, fallbacks...)
	return &Turn{Deps: Deps{Engine: &Engine{LLM: newTestLLM(), Chain: func(context.Context) []llm.ChatProvider { return chain }}}}
}

func TestNativeRoundAnthropicHeadTextAnswer(t *testing.T) {
	fa := (&fakeAnthropic{}).reply(200, `{"id":"m1","content":[{"type":"thinking","thinking":"","signature":"sig"},{"type":"text","text":"Hello there"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
	srv := httptest.NewServer(fa)
	defer srv.Close()
	tn := anthropicTurnFor(srv.URL)
	msgs := []map[string]any{{"role": "system", "content": "SYS"}, {"role": "user", "content": "hi"}}
	got := collect(t, tn.nativeRound(context.Background(), roundRequest{messages: msgs, toolsEnabled: true}))
	want := `[{"d":"Hello there","t":"content"},{"cursor":0,"t":"done"}]`
	if b, _ := json.Marshal(got); string(b) != want {
		t.Fatalf("frames %s, want %s", b, want)
	}
	body := fa.bodies[0]
	if body["max_tokens"] != float64(anthropicTokens) || body["model"] != "claude-opus-4-8" {
		t.Errorf("body %v", body)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != len(Catalog()) {
		t.Errorf("tools = %d, want %d", len(tools), len(Catalog()))
	}
	if first, _ := tools[0].(map[string]any); first["name"] != "search_leads" || first["input_schema"] == nil {
		t.Errorf("tool[0] = %v", first)
	}
	if sys, _ := json.Marshal(body["system"]); !strings.Contains(string(sys), `"text":"SYS"`) {
		t.Errorf("system = %s", sys)
	}
	if m, _ := json.Marshal(body["messages"]); string(m) != `[{"content":[{"text":"hi","type":"text"}],"role":"user"}]` {
		t.Errorf("messages = %s", m)
	}
}

func TestNativeRoundAnthropicToolUseReplaysRawContent(t *testing.T) {
	toolTurn := `{"id":"m1","content":[{"type":"thinking","thinking":"","signature":"sig-1"},{"type":"text","text":"Checking."},{"type":"tool_use","id":"toolu_1","name":"get_lead_stats","input":{}}],"stop_reason":"tool_use","usage":{}}`
	fa := (&fakeAnthropic{}).reply(200, toolTurn).reply(200, `{"id":"m2","content":[{"type":"text","text":"Done."}],"usage":{}}`)
	srv := httptest.NewServer(fa)
	defer srv.Close()
	tn := anthropicTurnFor(srv.URL)
	msgs := []map[string]any{{"role": "system", "content": "SYS"}, {"role": "user", "content": "stats?"}}
	got := collect(t, tn.nativeRound(context.Background(), roundRequest{messages: msgs, toolsEnabled: true}))
	b, _ := json.Marshal(got)
	if string(b) != `[{"d":"Checking.","t":"content"},{"cursor":0,"d":[{"function":{"arguments":"{}","name":"get_lead_stats"},"id":"toolu_1","type":"function"}],"t":"tool_calls"}]` {
		t.Fatalf("frames %s", b)
	}
	// The turn loop appends the assistant tool-call message, the tool result and the budget notice.
	calls, err := parseToolCalls(json.RawMessage(`[{"id":"toolu_1","type":"function","function":{"name":"get_lead_stats","arguments":"{}"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	msgs = append(msgs, map[string]any{"role": "assistant", "content": "", "tool_calls": callsAny(calls)},
		map[string]any{"role": "tool", "tool_call_id": "toolu_1", "name": "get_lead_stats", "content": `{"total": 1}`},
		map[string]any{"role": "system", "content": "You have reached the maximum"})
	got = collect(t, tn.nativeRound(context.Background(), roundRequest{messages: msgs, budgetHit: true}))
	if b, _ := json.Marshal(got); string(b) != `[{"d":"Done.","t":"content"},{"cursor":0,"t":"done"}]` {
		t.Fatalf("round 2 frames %s", b)
	}
	m, _ := json.Marshal(fa.bodies[1]["messages"])
	want := `[{"content":[{"text":"stats?","type":"text"}],"role":"user"},` +
		`{"content":[{"signature":"sig-1","thinking":"","type":"thinking"},{"text":"Checking.","type":"text"},{"id":"toolu_1","input":{},"name":"get_lead_stats","type":"tool_use"}],"role":"assistant"},` +
		`{"content":[{"content":"{\"total\": 1}","tool_use_id":"toolu_1","type":"tool_result"},{"text":"You have reached the maximum","type":"text"}],"role":"user"}]`
	if string(m) != want {
		t.Errorf("round 2 messages\n got %s\nwant %s", m, want)
	}
	if raw := `[{"type":"thinking","thinking":"","signature":"sig-1"},{"type":"text","text":"Checking."},{"type":"tool_use","id":"toolu_1","name":"get_lead_stats","input":{}}]`; !strings.Contains(fa.raw[1], raw) {
		t.Errorf("the assistant content is not replayed byte for byte: %s", fa.raw[1])
	}
	if tools, _ := fa.bodies[1]["tools"].([]any); len(tools) != 0 {
		t.Errorf("budget-hit round sent %d tools", len(tools))
	}
}

func TestNativeRoundAnthropicFailoverAndErrors(t *testing.T) {
	fa := (&fakeAnthropic{}).reply(429, `{"error":{"type":"rate_limit_error"}}`)
	srv := httptest.NewServer(fa)
	defer srv.Close()
	fp := &fakeProvider{script: map[string][]scriptedResp{"/p2/chat/completions": {{SSE: []string{`data: {"choices":[{"delta":{"content":"ok"}}]}`, "data: [DONE]"}}}}, counts: map[string]int{}}
	osrv := httptest.NewServer(fp)
	defer osrv.Close()
	groq := llm.ChatProvider{ID: "groq", Name: "Groq", APIKey: "sk-groq-123456", BaseURL: osrv.URL + "/p2", Model: "m", OpenAICompatible: true}

	got := collect(t, anthropicTurnFor(srv.URL, groq).nativeRound(context.Background(), roundRequest{messages: []map[string]any{{"role": "user", "content": "x"}}}))
	if b, _ := json.Marshal(got); string(b) != `[{"d":"⚡ Anthropic rate limited — switching to Groq...","t":"warning"},{"d":"ok","t":"content"},{"cursor":1,"t":"done"}]` {
		t.Fatalf("failover frames %s", b)
	}
	got = collect(t, anthropicTurnFor(srv.URL).nativeRound(context.Background(), roundRequest{messages: []map[string]any{{"role": "user", "content": "x"}}}))
	if b, _ := json.Marshal(got); !strings.HasPrefix(string(b), `[{"d":"Rate limited by Anthropic and no fallback providers available.`) || len(got) != 1 {
		t.Fatalf("no-fallback frames %s", b)
	}
	fa400 := (&fakeAnthropic{}).reply(400, `{"error":{"message":"bad"}}`)
	srv400 := httptest.NewServer(fa400)
	defer srv400.Close()
	got = collect(t, anthropicTurnFor(srv400.URL, groq).nativeRound(context.Background(), roundRequest{messages: []map[string]any{{"role": "user", "content": "x"}}}))
	if len(got) != 1 || got[0]["t"] != "error" || !strings.HasPrefix(got[0]["d"].(string), "Provider error 400: ") {
		t.Fatalf("400 frames %v", got)
	}
}

func TestNativeRoundSkipsOtherNonCompatibleHead(t *testing.T) {
	fp := &fakeProvider{script: map[string][]scriptedResp{"/p2/chat/completions": {{SSE: []string{"data: [DONE]"}}}}, counts: map[string]int{}}
	osrv := httptest.NewServer(fp)
	defer osrv.Close()
	cohere := llm.ChatProvider{ID: "cohere", Name: "Cohere", APIKey: "co-123456", BaseURL: osrv.URL + "/c", Model: "m"}
	groq := llm.ChatProvider{ID: "groq", Name: "Groq", APIKey: "sk-groq-123456", BaseURL: osrv.URL + "/p2", Model: "m", OpenAICompatible: true}
	run := func(chain ...llm.ChatProvider) string {
		tn := &Turn{Deps: Deps{Engine: &Engine{LLM: newTestLLM(), Chain: func(context.Context) []llm.ChatProvider { return chain }}}}
		b, _ := json.Marshal(collect(t, tn.nativeRound(context.Background(), roundRequest{messages: []map[string]any{{"role": "user", "content": "x"}}})))
		return string(b)
	}
	if got := run(cohere, groq); got != `[{"d":"⚡ Cohere does not support streamed chat — switching to Groq...","t":"warning"},{"cursor":1,"t":"done"}]` {
		t.Errorf("skip frames %s", got)
	}
	if got := run(cohere); got != `[{"d":"Cohere cannot serve the copilot chat (it is not OpenAI-compatible). Choose another default AI provider in Settings.","t":"error"}]` {
		t.Errorf("no-fallback frames %s", got)
	}
	for _, r := range fp.log {
		if strings.HasPrefix(r.Path, "/c") {
			t.Errorf("the non-compatible provider was called: %s", r.Path)
		}
	}
}
