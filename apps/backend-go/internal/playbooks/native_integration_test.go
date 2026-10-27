//go:build integration

package playbooks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/aicol"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
)

// nativeLLM serves the native research path: one end_turn turn, then the structured synthesis.
type nativeLLM struct {
	mu    sync.Mutex
	turns int
	qs    []string
}

func (n *nativeLLM) Complete(context.Context, llm.CompleteRequest) (string, error) {
	return "", llm.ErrEmptyResponse
}
func (n *nativeLLM) ExtractJSON(context.Context, string, string, int) (map[string]any, error) {
	return nil, llm.ErrInvalidJSON
}
func (n *nativeLLM) AnthropicProvider(context.Context) (llm.Provider, bool) {
	return llm.Provider{ID: "anthropic", Model: "claude-sonnet-4-6", Native: true}, true
}
func (n *nativeLLM) BatchComplete(context.Context, []llm.BatchRequest, llm.BatchOptions) (map[string]string, error) {
	return nil, errors.New("unused")
}

func (n *nativeLLM) AnthropicTurn(_ context.Context, r llm.AnthropicRequest) (*llm.AnthropicResponse, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.turns++
	if r.OutputFormat == nil {
		first, _ := r.Messages[0].(map[string]any)
		q, _ := first["content"].(string)
		n.qs = append(n.qs, q)
		return &llm.AnthropicResponse{StopReason: "end_turn", Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"x"}`)},
			Usage: llm.AnthropicUsage{InputTokens: 1000, OutputTokens: 200}}, nil
	}
	body, _ := json.Marshal(map[string]any{"type": "text", "text": `{"answer": " Native answer ", "citations": []}`})
	return &llm.AnthropicResponse{StopReason: "end_turn", Content: []json.RawMessage{body},
		Usage: llm.AnthropicUsage{InputTokens: 1000, OutputTokens: 200}}, nil
}

type flagSettings map[string]string

func (f flagSettings) Get(_ context.Context, k, def string) (string, error) {
	if v, ok := f[k]; ok {
		return v, nil
	}
	return def, nil
}

// A playbook run with the native researcher needs no Python worker: every step runs the Go
// research agent and the ledger keeps the research metadata in Python's key order.
func TestRunNativeResearcherWithoutPyworker(t *testing.T) {
	e := setup(t)
	e.members(t, 10, 20)
	_, run := e.engineRun(t, `[{"key":"s1","name":"S1","prompt_template":"Who runs {company}?","output_format":"text"},
{"key":"s2","name":"S2","prompt_template":"Follow up on {s1}","output_format":"text"}]`, 100)
	n := &nativeLLM{}
	var wsSeen []string
	var mu sync.Mutex
	res := NewNativeResearcher(func(_ context.Context, ws string) aicol.Deps {
		mu.Lock()
		wsSeen = append(wsSeen, ws)
		mu.Unlock()
		return aicol.Deps{LLM: n, Settings: flagSettings{"RESEARCH_NATIVE_TOOLS": "1"}}
	}, 0)
	if err := e.run(t, res, run); err != nil {
		t.Fatal(err)
	}
	if n.turns != 8 || len(wsSeen) != 4 || wsSeen[0] != e.ws {
		t.Fatalf("turns %d workspaces %v", n.turns, wsSeen)
	}
	if !strings.Contains(n.qs[1], "Follow up on ") || !strings.Contains(n.qs[1], "Native answer") {
		t.Fatalf("step 2 question %q", n.qs[1])
	}
	rs := e.results(t, e.ws, run.ID)
	if len(rs) != 2 || rs[0].status != "success" || rs[0].value != "Native answer" || rs[0].err != nil {
		t.Fatalf("results %+v", rs)
	}
	want := `{"steps":[{"key":"s1","name":"S1","success":true,"value":"Native answer","metadata":{"research":{"answer":"Native answer","citations":[],"cost_usd":0.012,"stopped_reason":"answered","synthesis_fallback":false}},"error":null}`
	if !strings.HasPrefix(rs[0].meta, want) {
		t.Fatalf("meta %s", rs[0].meta)
	}
	if st := e.runState(t, e.ws, run.ID); st.status != "completed" || st.succeeded != 2 {
		t.Fatalf("run %+v", st)
	}
}
