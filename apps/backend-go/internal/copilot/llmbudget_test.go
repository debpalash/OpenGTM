package copilot

import (
	"context"
	"errors"
	"sync"
	"testing"
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
