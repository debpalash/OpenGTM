//go:build integration

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// pyGuard is the stand-in for the Python api/worker (PY_WORKER_URL, INTERNAL_API_URL). Every request is recorded
// with the scenario that was running and answered 503; the parent test fails when any were seen unless the scenario
// is in the known-failing report mode.
type pyGuard struct {
	srv *httptest.Server
	mu  sync.Mutex
	cur string
	hit map[string][]string // scenario -> "METHOD path"
}

func newPyGuard() *pyGuard {
	g := &pyGuard{hit: map[string][]string{}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		g.mu.Lock()
		g.hit[g.cur] = append(g.hit[g.cur], r.Method+" "+r.URL.Path)
		g.mu.Unlock()
		http.Error(w, `{"detail":"python is not available in the no-python harness"}`, http.StatusServiceUnavailable)
	}))
	return g
}

func (g *pyGuard) scenario(name string) {
	g.mu.Lock()
	g.cur = name
	g.mu.Unlock()
}

func (g *pyGuard) calls(name string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.hit[name]...)
}

// fakeBrowser implements the browserclient contract (apps/browser-service openapi): bearer auth, the /v1/* ops, answers
// replayed from internal/browserclient/testdata/recorded (responses recorded from the real service).
type fakeBrowser struct {
	srv   *httptest.Server
	token string
	mu    sync.Mutex
	hits  []string
}

func newFakeBrowser(t *testing.T, token string) *fakeBrowser {
	t.Helper()
	dir := filepath.Join("..", "browserclient", "testdata", "recorded")
	byPath := map[string]json.RawMessage{}
	status := map[string]int{}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, f := range files {
		var rec struct {
			Path     string          `json:"path"`
			Status   int             `json:"status"`
			Response json.RawMessage `json:"response"`
		}
		b, _ := os.ReadFile(f)
		if json.Unmarshal(b, &rec) != nil || rec.Path == "" || strings.Contains(f, "error") || strings.Contains(f, "bad_proxy") {
			continue
		}
		if _, dup := byPath[rec.Path]; !dup {
			byPath[rec.Path] = rec.Response
			status[rec.Path] = rec.Status
		}
	}
	fb := &fakeBrowser{token: token}
	fb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		fb.mu.Lock()
		fb.hits = append(fb.hits, r.Method+" "+r.URL.Path)
		fb.mu.Unlock()
		if r.URL.Path == "/healthz" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+fb.token {
			http.Error(w, `{"detail":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		resp, ok := byPath[r.URL.Path]
		if !ok {
			http.Error(w, `{"detail":"no recorded response"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if s := status[r.URL.Path]; s > 0 {
			w.WriteHeader(s)
		}
		_, _ = w.Write(resp)
	}))
	t.Cleanup(fb.srv.Close)
	return fb
}

// fakeLLM is an OpenAI-compatible /chat/completions endpoint. A request that carries tools and has no tool result
// yet answers with a tool call to toolName; otherwise it answers with text (streamed as SSE when asked).
type fakeLLM struct {
	srv      *httptest.Server
	toolName string
	toolArgs string
	mu       sync.Mutex
	reqs     int
}

func newFakeLLM(t *testing.T) *fakeLLM {
	t.Helper()
	l := &fakeLLM{}
	l.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		l.mu.Lock()
		l.reqs++
		tool, args := l.toolName, l.toolArgs
		l.mu.Unlock()
		var req struct {
			Stream   bool              `json:"stream"`
			Tools    []json.RawMessage `json:"tools"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &req)
		hasToolResult := false
		for _, m := range req.Messages {
			if m.Role == "tool" {
				hasToolResult = true
			}
		}
		callTool := len(req.Tools) > 0 && !hasToolResult && tool != ""
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			if callTool {
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", tool, args)
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n")
			} else {
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"All done.\"},\"finish_reason\":null}]}\n\n")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\n")
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		msg := `{"role":"assistant","content":"fake summary"}`
		finish := "stop"
		if callTool {
			msg = fmt.Sprintf(`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":%q,"arguments":%q}}]}`, tool, args)
			finish = "tool_calls"
		}
		fmt.Fprintf(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":%s,"finish_reason":%q}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`, msg, finish)
	}))
	t.Cleanup(l.srv.Close)
	return l
}

func (l *fakeLLM) setTool(name, args string) {
	l.mu.Lock()
	l.toolName, l.toolArgs = name, args
	l.mu.Unlock()
}
