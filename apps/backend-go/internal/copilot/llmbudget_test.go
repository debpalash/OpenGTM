package copilot

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
)

// fakeBudget admits `allow` rounds, then refuses; it records every bid and settlement.
type fakeBudget struct {
	mu      sync.Mutex
	allow   int
	err     error
	bids    []RoundBid
	settled []RoundCharge
}

func (b *fakeBudget) Reserve(_ context.Context, bid RoundBid) (RoundHold, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	b.bids = append(b.bids, bid)
	if len(b.bids) > b.allow {
		return nil, &BudgetExceededError{Reason: "cell_budget"}
	}
	return fakeHold{b}, nil
}

type fakeHold struct{ b *fakeBudget }

func (h fakeHold) Settle(_ context.Context, c RoundCharge) {
	h.b.mu.Lock()
	h.b.settled = append(h.b.settled, c)
	h.b.mu.Unlock()
}

// Final security M3: every llm round is reserved against the workspace's daily LLM budget before the provider is
// called and settled after it; a refused round ends the turn with a clear error frame and calls no provider.
func TestRoundBudgetReservesAndSettlesEveryRound(t *testing.T) {
	f := newFakeLLM(t,
		[]string{`{"t":"tool_calls","d":[{"id":"c1","type":"function","function":{"name":"search_leads","arguments":"{\"query\": \"acme\"}"}}],"cursor":0}`},
		[]string{`{"t":"content","d":"answer"}`, `{"t":"done","cursor":0}`})
	b := &fakeBudget{allow: 10}
	tn := newTurn(f, nativeConst(`{"leads": [], "count": 0}`))
	tn.Budget = b
	if _, err := runTurn(t, tn, userMsgs()); err != nil {
		t.Fatal(err)
	}
	if len(b.bids) != 2 || len(b.settled) != 2 {
		t.Fatalf("bids=%d settled=%d", len(b.bids), len(b.settled))
	}
	for i, bid := range b.bids {
		if bid.WorkspaceID != "ws-t13" || bid.ConvID != "c1" || bid.ExposureMicroUSD <= 0 {
			t.Fatalf("bid %d: %+v", i, bid)
		}
	}
	for i, c := range b.settled {
		if c.MicroUSD <= 0 || !c.Estimated || c.PromptTokens <= 0 {
			t.Fatalf("settle %d: %+v", i, c)
		}
	}
	// The second round carried the tool result, so it costs more input.
	if b.bids[1].ExposureMicroUSD <= b.bids[0].ExposureMicroUSD {
		t.Fatalf("exposure did not grow: %+v", b.bids)
	}
}

func TestRoundBudgetExhaustedEndsTurnWithErrorFrame(t *testing.T) {
	f := newFakeLLM(t, []string{`{"t":"content","d":"never"}`, `{"t":"done","cursor":0}`})
	tn := newTurn(f, nil)
	tn.Budget = &fakeBudget{allow: 0}
	body, _ := runTurn(t, tn, userMsgs())
	if want := frame(`{"error": "`+ErrTextLLMBudget+`"}`) + done; body != want {
		t.Fatalf("got %q want %q", body, want)
	}
	if len(f.llmReqs) != 0 {
		t.Fatal("a provider round ran after the budget refused")
	}
}

// A budget that cannot be read fails closed: no provider call.
func TestRoundBudgetErrorFailsClosed(t *testing.T) {
	f := newFakeLLM(t, []string{`{"t":"content","d":"never"}`, `{"t":"done","cursor":0}`})
	tn := newTurn(f, nil)
	tn.Budget = &fakeBudget{err: errors.New("db down")}
	body, _ := runTurn(t, tn, userMsgs())
	if want := frame(`{"error": "`+errTextInterrupted+`"}`) + done; body != want {
		t.Fatalf("got %q want %q", body, want)
	}
	if len(f.llmReqs) != 0 {
		t.Fatal("a provider round ran without an admission")
	}
}

type fakePlannerLLM struct {
	calls int
	resp  *llm.AnthropicResponse
	err   error
}

func (f *fakePlannerLLM) AnthropicProvider(context.Context) (llm.Provider, bool) {
	return llm.Provider{ID: "anthropic", Model: "claude-sonnet-4-5"}, true
}
func (f *fakePlannerLLM) AnthropicTurn(context.Context, llm.AnthropicRequest) (*llm.AnthropicResponse, error) {
	f.calls++
	return f.resp, f.err
}

// Follow-up to final security M3: the non-chat llm call (the autopilot planner behind draft_plan) goes through
// the same workspace budget: reserved before the provider is called, settled from the reported usage.
func TestPlannerLLMReservesAndSettles(t *testing.T) {
	inner := &fakePlannerLLM{resp: &llm.AnthropicResponse{Usage: llm.AnthropicUsage{InputTokens: 120, OutputTokens: 30}}}
	b := &fakeBudget{allow: 5}
	m := &meteredLLM{inner: inner, budget: b, ws: "ws-t13", conv: "c1"}
	if _, err := m.AnthropicTurn(context.Background(), llm.AnthropicRequest{
		Messages: []any{map[string]any{"role": "user", "content": "plan it"}}, MaxTokens: 1024}); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 || len(b.bids) != 1 || len(b.settled) != 1 {
		t.Fatalf("calls=%d bids=%d settled=%d", inner.calls, len(b.bids), len(b.settled))
	}
	if b.bids[0].WorkspaceID != "ws-t13" || b.bids[0].ExposureMicroUSD <= 0 {
		t.Fatalf("bid %+v", b.bids[0])
	}
	if c := b.settled[0]; c.PromptTokens != 120 || c.CompletionTokens != 30 || c.MicroUSD <= 0 || c.Estimated {
		t.Fatalf("charge %+v", c)
	}
}

func TestPlannerLLMRefusedBudgetCallsNoProvider(t *testing.T) {
	inner := &fakePlannerLLM{resp: &llm.AnthropicResponse{}}
	m := &meteredLLM{inner: inner, budget: &fakeBudget{allow: 0}, ws: "ws-t13"}
	_, err := m.AnthropicTurn(context.Background(), llm.AnthropicRequest{MaxTokens: 1024})
	var be *BudgetExceededError
	if !errors.As(err, &be) || inner.calls != 0 {
		t.Fatalf("err=%v calls=%d", err, inner.calls)
	}
}

func TestPlannerLLMFailedCallStillSettles(t *testing.T) {
	inner := &fakePlannerLLM{err: errors.New("boom")}
	b := &fakeBudget{allow: 1}
	m := &meteredLLM{inner: inner, budget: b, ws: "ws-t13"}
	if _, err := m.AnthropicTurn(context.Background(), llm.AnthropicRequest{MaxTokens: 1024}); err == nil {
		t.Fatal("want error")
	}
	if len(b.settled) != 1 {
		t.Fatalf("settled %d: a failed call must release its hold", len(b.settled))
	}
}

func TestTurnPlannerUsesBudgetedLLM(t *testing.T) {
	tn := &Turn{Deps: Deps{Settings: settingsStub{}, Engine: &Engine{LLM: llm.New(llm.Options{Settings: mapSettings{}, HTTP: &http.Client{}})}, Budget: &fakeBudget{allow: 1}}, WS: "ws-t13", ConvID: "c1"}
	if _, ok := tn.planner().LLM.(*meteredLLM); !ok {
		t.Fatalf("planner LLM %T is not metered", tn.planner().LLM)
	}
}
