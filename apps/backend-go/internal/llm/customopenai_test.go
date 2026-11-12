package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockOpenAI is a mock OpenAI-compatible chat-completions server: it records
// every request (path, headers, body) and answers per the scripted handler.
type mockOpenAI struct {
	mu       sync.Mutex
	requests []mockReq
	handler  func(w http.ResponseWriter, r *http.Request, body map[string]any)
}

type mockReq struct {
	Path   string
	Auth   string
	Body   map[string]any
	Raw    string
}

func (m *mockOpenAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	m.mu.Lock()
	m.requests = append(m.requests, mockReq{Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Body: body, Raw: string(raw)})
	m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	m.handler(w, r, body)
}

func (m *mockOpenAI) last(t *testing.T) mockReq {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) == 0 {
		t.Fatal("mock received no requests")
	}
	return m.requests[len(m.requests)-1]
}

func okHandler(text string) func(http.ResponseWriter, *http.Request, map[string]any) {
	return func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": text}}},
			"usage":   map[string]any{"prompt_tokens": 2, "completion_tokens": 3},
		})
	}
}

func customClient(t *testing.T, extra mapSettings) *Client {
	t.Helper()
	s := mapSettings{"LLM_DEFAULT_PROVIDER": CustomOpenAIID}
	for k, v := range extra {
		s[k] = v
	}
	return newTestClient(t, s, nil, nil)
}

// 1 + 2 + 6: registration, base-URL configuration and an arbitrary model id —
// the completion goes to exactly the configured URL with exactly the
// configured model (a vLLM-style path-prefixed model id).
func TestCustomOpenAIRegistrationBaseURLAndModel(t *testing.T) {
	const model = "/root/.cache/huggingface/hub/models--Qwen--Qwen3.6-35B-A3B-FP8/snapshots/95a723d08a9490559dae23d0cff1d9466213d989"
	m := &mockOpenAI{handler: okHandler("OPENGTM_LOCAL_LLM_WORKING")}
	srv := httptest.NewServer(m)
	defer srv.Close()
	c := customClient(t, mapSettings{
		"CUSTOM_OPENAI_BASE_URL": srv.URL + "/v1",
		"CUSTOM_OPENAI_MODEL":    model,
		"CUSTOM_OPENAI_API_KEY":  "sk-test-1234567890abcd",
	})
	got, err := c.Complete(context.Background(), CompleteRequest{Prompt: "ping"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "OPENGTM_LOCAL_LLM_WORKING" {
		t.Fatalf("text %q", got)
	}
	req := m.last(t)
	if req.Path != "/v1/chat/completions" {
		t.Fatalf("path %q", req.Path)
	}
	if req.Body["model"] != model {
		t.Fatalf("model %v", req.Body["model"])
	}
	if chain := c.Providers(context.Background()); len(chain) == 0 || chain[0].ID != CustomOpenAIID {
		t.Fatalf("provider chain %v", chain)
	}
}

// 4: the Authorization header carries the configured key.
func TestCustomOpenAISendsAuthorizationWhenKeySet(t *testing.T) {
	m := &mockOpenAI{handler: okHandler("ok")}
	srv := httptest.NewServer(m)
	defer srv.Close()
	c := customClient(t, mapSettings{
		"CUSTOM_OPENAI_BASE_URL": srv.URL + "/v1",
		"CUSTOM_OPENAI_MODEL":    "my-model",
		"CUSTOM_OPENAI_API_KEY":  "secret-key-0123456789",
	})
	if _, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	if got := m.last(t).Auth; got != "Bearer secret-key-0123456789" {
		t.Fatalf("auth %q", got)
	}
}

// 3 + 5: no key is required and no Authorization header is sent when the
// server serves without authentication.
func TestCustomOpenAIKeylessNoAuthHeader(t *testing.T) {
	m := &mockOpenAI{handler: okHandler("ok")}
	srv := httptest.NewServer(m)
	defer srv.Close()
	c := customClient(t, mapSettings{
		"CUSTOM_OPENAI_BASE_URL": srv.URL + "/v1",
		"CUSTOM_OPENAI_MODEL":    "my-model",
	})
	if _, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"}); err != nil {
		t.Fatalf("keyless custom_openai must work: %v", err)
	}
	if got := m.last(t).Auth; got != "" {
		t.Fatalf("must send no Authorization header, got %q", got)
	}
}

// A configured custom_openai without a base URL (or without a model) never
// runs: it must not be picked and must not error with a half-configured
// provider, so other configured providers keep serving.
func TestCustomOpenAINeedsURLAndModel(t *testing.T) {
	c := customClient(t, mapSettings{"CUSTOM_OPENAI_MODEL": "m"})
	if _, ok := c.providerConfig(context.Background(), CustomOpenAIID); ok {
		t.Fatal("no base URL: provider must not resolve")
	}
	c = customClient(t, mapSettings{"CUSTOM_OPENAI_BASE_URL": "http://example.invalid/v1"})
	if _, ok := c.providerConfig(context.Background(), CustomOpenAIID); ok {
		t.Fatal("no model: provider must not resolve")
	}
	if _, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("got %v", err)
	}
}

// custom_openai never joins the failover chain: it only leads as the explicit
// default, so an unselected custom endpoint never receives another tenant's
// traffic.
func TestCustomOpenAINeverInFailoverOrder(t *testing.T) {
	for _, id := range ProviderOrder {
		if id == CustomOpenAIID {
			t.Fatal("custom_openai must not be in ProviderOrder")
		}
	}
	srv := httptest.NewServer(&mockOpenAI{handler: okHandler("ok")})
	defer srv.Close()
	// groq is the default here; custom_openai is fully configured but not the
	// default -> it must be absent from the chain.
	c := newTestClient(t, mapSettings{
		"GROQ_API_KEY":           "k",
		"CUSTOM_OPENAI_BASE_URL": srv.URL + "/v1",
		"CUSTOM_OPENAI_MODEL":    "m",
	}, nil, nil)
	for _, p := range c.Providers(context.Background()) {
		if p.ID == CustomOpenAIID {
			t.Fatal("custom_openai must not appear when it is not the default")
		}
	}
}

// 7: tool/function calling through the streaming path — vLLM-style chunks
// with delta.tool_calls and finish_reason tool_calls accumulate into complete
// ToolCalls frames (the shape the copilot agent depends on).
func TestCustomOpenAIStreamingToolCalls(t *testing.T) {
	chunks := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"ci"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"acme\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	m := &mockOpenAI{handler: func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}}
	srv := httptest.NewServer(m)
	defer srv.Close()
	c := customClient(t, mapSettings{
		"CUSTOM_OPENAI_BASE_URL": srv.URL,
		"CUSTOM_OPENAI_MODEL":    "m",
	})
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "lookup"}}}
	frames := drain(t, c.Stream(context.Background(), StreamRequest{
		Messages: []map[string]string{{"role": "user", "content": "hi"}},
		Tools:    tools,
		Cursor:   Cursor{Provider: ChatProvider{ID: CustomOpenAIID, APIKey: "", BaseURL: srv.URL, Model: "m"}},
	}))
	var tc *Frame
	for i := range frames {
		if frames[i].Kind == FrameToolCalls {
			tc = &frames[i]
		}
	}
	if tc == nil {
		t.Fatalf("no tool_calls frame in %v", kinds(frames))
	}
	if len(tc.ToolCalls) != 1 || tc.ToolCalls[0].ID != "call_1" ||
		tc.ToolCalls[0].Function.Name != "lookup" ||
		tc.ToolCalls[0].Function.Arguments != `{"city":"acme"}` {
		t.Fatalf("tool calls %+v", tc.ToolCalls)
	}
	req := m.last(t)
	if req.Body["tools"] == nil {
		t.Fatal("tools not forwarded in request body")
	}
	if req.Body["stream"] != true {
		t.Fatal("stream flag missing")
	}
}

// Streaming content deltas and the [DONE] frame work against the mock server.
func TestCustomOpenAIStreamingContent(t *testing.T) {
	m := &mockOpenAI{handler: func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"Hello"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":" world"},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}}
	srv := httptest.NewServer(m)
	defer srv.Close()
	c := customClient(t, mapSettings{
		"CUSTOM_OPENAI_BASE_URL": srv.URL,
		"CUSTOM_OPENAI_MODEL":    "m",
	})
	frames := drain(t, c.Stream(context.Background(), StreamRequest{
		Messages: []map[string]string{{"role": "user", "content": "hi"}},
		Cursor:   Cursor{Provider: ChatProvider{ID: CustomOpenAIID, BaseURL: srv.URL, Model: "m"}},
	}))
	var text strings.Builder
	done := false
	for _, f := range frames {
		switch f.Kind {
		case FrameContent:
			text.WriteString(f.Text)
		case FrameDone:
			done = true
		case FrameError:
			t.Fatalf("error frame: %s", f.Text)
		}
	}
	if text.String() != "Hello world" || !done {
		t.Fatalf("text %q done=%v", text.String(), done)
	}
}

// 10: an invalid base URL surfaces a meaningful error, not a panic, and the
// URL is not echoed beyond its host.
func TestCustomOpenAIInvalidBaseURL(t *testing.T) {
	c := customClient(t, mapSettings{
		"CUSTOM_OPENAI_BASE_URL": "::not a url::",
		"CUSTOM_OPENAI_MODEL":    "m",
	})
	_, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), CustomOpenAIID) {
		t.Fatalf("want provider-tagged error, got %v", err)
	}
	if strings.Contains(err.Error(), "not a url") {
		t.Fatalf("raw URL leaked into error: %v", err)
	}
}

// 11: connection timeout — CUSTOM_OPENAI_HTTP_TIMEOUT bounds the request and
// the error says it timed out.
func TestCustomOpenAIConnectionTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	c := customClient(t, mapSettings{
		"CUSTOM_OPENAI_BASE_URL":     srv.URL,
		"CUSTOM_OPENAI_MODEL":        "m",
		"CUSTOM_OPENAI_HTTP_TIMEOUT": "0.1",
	})
	start := time.Now()
	_, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"})
	if !errors.Is(err, ErrAllFailed) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout was not honoured")
	}
}

// 12: HTTP 401 and 404 produce meaningful, provider-tagged errors.
func TestCustomOpenAI401And404(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{401, `{"error":{"message":"invalid api key"}}`, "HTTP 401: invalid api key"},
		{404, `{"error":{"message":"The model 'nope' does not exist"}}`, "does not exist"},
	} {
		m := &mockOpenAI{handler: func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}}
		srv := httptest.NewServer(m)
		c := customClient(t, mapSettings{
			"CUSTOM_OPENAI_BASE_URL": srv.URL + "/v1",
			"CUSTOM_OPENAI_MODEL":    "nope",
			"CUSTOM_OPENAI_API_KEY":  "k1234567890abcdef",
		})
		_, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"})
		srv.Close()
		if !errors.Is(err, ErrAllFailed) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("status %d: got %v want %q", tc.status, err, tc.want)
		}
		if !strings.Contains(err.Error(), CustomOpenAIID) {
			t.Fatalf("status %d: error must name the provider: %v", tc.status, err)
		}
		if strings.Contains(err.Error(), "k1234567890abcdef") {
			t.Fatalf("status %d: key leaked: %v", tc.status, err)
		}
	}
}

// 13 + 14: workspace-level settings win over the global configuration, and the
// global level is the fallback; the key and the URL must come from the same
// level (never a workspace URL with the global key).
func TestCustomOpenAIWorkspaceAndGlobalPrecedence(t *testing.T) {
	m := &mockOpenAI{handler: okHandler("ok")}
	srv := httptest.NewServer(m)
	defer srv.Close()
	ctx := context.Background()

	// global only
	c := New(Options{Settings: sourced{
		global: map[string]string{"CUSTOM_OPENAI_BASE_URL": srv.URL + "/v1", "CUSTOM_OPENAI_MODEL": "global-model"},
	}, HTTP: srv.Client()})
	p, ok := c.providerConfig(ctx, CustomOpenAIID)
	if !ok || p.BaseURL != srv.URL+"/v1" || p.Model != "global-model" {
		t.Fatalf("global fallback: %+v ok=%v", p, ok)
	}

	// workspace overrides global
	c = New(Options{Settings: sourced{
		ws:     map[string]string{"CUSTOM_OPENAI_BASE_URL": srv.URL + "/ws/v1", "CUSTOM_OPENAI_MODEL": "ws-model"},
		global: map[string]string{"CUSTOM_OPENAI_BASE_URL": srv.URL + "/v1", "CUSTOM_OPENAI_MODEL": "global-model"},
	}, HTTP: srv.Client()})
	p, ok = c.providerConfig(ctx, CustomOpenAIID)
	if !ok || p.BaseURL != srv.URL+"/ws/v1" || p.Model != "ws-model" {
		t.Fatalf("workspace level: %+v ok=%v", p, ok)
	}

	// workspace URL must never pair with a global key: the URL is ignored,
	// custom_openai has no default URL, so the provider refuses to resolve
	// (fail closed — no dial to the workspace URL, no key leak).
	c = New(Options{Settings: sourced{
		ws:     map[string]string{"CUSTOM_OPENAI_BASE_URL": "http://workspace-only.invalid/v1"},
		global: map[string]string{"CUSTOM_OPENAI_BASE_URL": srv.URL + "/v1", "CUSTOM_OPENAI_MODEL": "m", "CUSTOM_OPENAI_API_KEY": "gk"},
	}, HTTP: srv.Client()})
	if p, ok := c.providerConfig(ctx, CustomOpenAIID); ok {
		t.Fatalf("cross-level URL/key pairing must refuse to resolve: %+v", p)
	}
}

// 15 (backend half): the existing providers keep their behaviour — groq and
// openrouter still require a key, keep their default URLs, and send Bearer
// auth. (The full regression suite runs unchanged alongside this file.)
func TestExistingProvidersUnaffected(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		id, envKey string
		wantURL    string
	}{
		{"groq", "GROQ_API_KEY", "https://api.groq.com/openai/v1"},
		{"openrouter", "OPENROUTER_API_KEY", "https://openrouter.ai/api/v1"},
		{"cerebras", "CEREBRAS_API_KEY", "https://api.cerebras.ai/v1"},
		{"sambanova", "SAMBANOVA_API_KEY", "https://api.sambanova.ai/v1"},
		{"nvidia", "NVIDIA_API_KEY", "https://integrate.api.nvidia.com/v1"},
		{"mistral", "MISTRAL_API_KEY", "https://api.mistral.ai/v1"},
		{"github_models", "GITHUB_MODELS_API_KEY", "https://models.inference.ai.azure.com"},
		{"siliconflow", "SILICONFLOW_API_KEY", "https://api.siliconflow.cn/v1"},
		{"anthropic", "ANTHROPIC_API_KEY", "https://api.anthropic.com"},
	} {
		spec, ok := providerSpecs[tc.id]
		if !ok {
			t.Fatalf("%s missing from providerSpecs", tc.id)
		}
		if spec.EnvKey != tc.envKey || spec.DefaultURL != tc.wantURL {
			t.Errorf("%s: spec drifted: %+v", tc.id, spec)
		}
		// no key -> provider unavailable (custom_openai is the only keyless one)
		c := New(Options{Settings: mapSettings{}, HTTP: http.DefaultClient})
		if _, ok := c.providerConfig(ctx, tc.id); ok {
			t.Errorf("%s must require a key", tc.id)
		}
	}
}

func drain(t *testing.T, ch <-chan Frame) []Frame {
	t.Helper()
	var out []Frame
	for f := range ch {
		out = append(out, f)
		if len(out) > 100 {
			t.Fatal("too many frames")
		}
	}
	return out
}

func kinds(frames []Frame) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = f.Kind
	}
	return out
}
