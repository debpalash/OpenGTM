package aicol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
)

// LLM spend accounting (review wave 3b M1). Every LLM call of an AI column (ai_formula,
// research turns and synthesis, the ai_formula batch) is reserved before it is made and
// settled after it through the workbook spend ledger, like a paid provider: a refused
// reservation means the call is not made; a call that fails or is cut off with no observed
// usage leaves its attempt uncertain (it may have been billed).

// Spend is the workbook spend ledger as one cell sees it. nil (a playbook step, which has
// no workbook) skips the ledger; the research budget still applies.
type Spend interface {
	// Begin reserves and dispatches one LLM call of the cell with the given hold. A refusal
	// is a *SpendRefused; any other error (lease lost, database) also means "do not call".
	Begin(ctx context.Context, call string, exposureMicroUSD int64) (SpendHold, error)
}

// SpendHold is a dispatched LLM attempt.
type SpendHold interface {
	// Settle records the call's charge (micro-USD) once; a failed settlement leaves the
	// attempt uncertain.
	Settle(ctx context.Context, chargedMicroUSD int64, receipt map[string]any)
	// Uncertain marks an attempt whose charge is unknown.
	Uncertain(ctx context.Context)
}

// SpendRefused is a reservation the ledger refused (workbook_budget, cell_budget, ...).
type SpendRefused struct{ Reason string }

func (e *SpendRefused) Error() string { return "llm spend refused: " + e.Reason }

// ErrSpend wraps a ledger failure that is not a refusal: the call was not made.
var ErrSpend = errors.New("llm spend ledger unavailable")

// usageSink collects the usage the LLM client reports while one accounted call runs.
type usageSink struct {
	mu    sync.Mutex
	micro float64
	n     int
}

type usageSinkKey struct{}

// UsageTap is the llm.UsageRecorder of the column LLM clients: it reports each provider
// call's usage to the accounted aicol call in flight (via ctx) and then to Next.
type UsageTap struct{ Next llm.UsageRecorder }

// Record implements llm.UsageRecorder.
func (t UsageTap) Record(ctx context.Context, workspaceID string, r llm.UsageRecord) error {
	if s, ok := ctx.Value(usageSinkKey{}).(*usageSink); ok && s != nil {
		in, out := llm.PriceForModel(r.Model)
		s.mu.Lock()
		s.micro += float64(r.PromptTokens)*in + float64(r.CompletionTokens)*out
		s.n++
		s.mu.Unlock()
	}
	if t.Next == nil {
		return nil
	}
	return t.Next.Record(ctx, workspaceID, r)
}

// estimateMicroUSD is the hold of one call: input at ~3 bytes per token plus the full output
// allowance, times the client's attempts, at the model's price (an unknown model, as for
// the provider chain of Complete, is priced at the most expensive tier).
func estimateMicroUSD(model string, inputBytes, maxTokens, attempts int) int64 {
	in, out := llm.PriceForModel(model)
	tokensIn := float64(inputBytes)/3 + 1
	return int64(math.Ceil(float64(max(1, attempts)) * (tokensIn*in + float64(max(0, maxTokens))*out)))
}

func jsonLen(v any) int {
	b, _ := json.Marshal(v)
	return len(b)
}

// llmCall runs fn as one accounted LLM call. fn returns the cost of its response when the
// response carries usage (AnthropicTurn), else -1. It returns the USD the call is charged
// (what the ledger was settled with; observed usage when the client reported it, else the
// hold when there is a ledger) and an error: a *SpendRefused or ErrSpend (fn never ran), or
// fn's own error.
func (d Deps) llmCall(ctx context.Context, call string, exposure int64, fn func(ctx context.Context) (float64, error)) (float64, error) {
	var hold SpendHold
	if d.Spend != nil {
		h, err := d.Spend.Begin(ctx, call, exposure)
		if err != nil {
			var ref *SpendRefused
			if errors.As(err, &ref) || ctx.Err() != nil {
				return 0, err
			}
			return 0, fmt.Errorf("%w: %w", ErrSpend, err)
		}
		hold = h
	}
	sink := &usageSink{}
	known, ferr := fn(context.WithValue(ctx, usageSinkKey{}, sink))
	sink.mu.Lock()
	observedMicro, n := sink.micro, sink.n
	sink.mu.Unlock()
	observed := false
	var micro float64
	switch {
	case known >= 0:
		micro, observed = known*1e6, true
	case n > 0:
		micro, observed = observedMicro, true
	}
	if hold == nil {
		return micro / 1e6, ferr
	}
	switch {
	case ctx.Err() != nil && !observed:
		hold.Uncertain(context.WithoutCancel(ctx))
		return 0, ferr
	case observed:
		charged := int64(math.Ceil(micro))
		hold.Settle(context.WithoutCancel(ctx), charged, map[string]any{"call": call, "basis": "observed_usage",
			"charged_microusd": charged, "error": ferr != nil})
		return float64(charged) / 1e6, ferr
	case ferr == nil:
		hold.Settle(context.WithoutCancel(ctx), exposure, map[string]any{"call": call, "basis": "reserved_estimate",
			"charged_microusd": exposure})
		return float64(exposure) / 1e6, nil
	default:
		hold.Uncertain(context.WithoutCancel(ctx))
		return 0, ferr
	}
}

// isSpendStop reports an llmCall error that means the call was never made.
func isSpendStop(err error) bool {
	var ref *SpendRefused
	return errors.As(err, &ref) || errors.Is(err, ErrSpend)
}

// spendText is the cell error of a refused call.
func spendText(err error) string {
	var ref *SpendRefused
	if errors.As(err, &ref) {
		return "llm_spend_refused: " + ref.Reason
	}
	return "llm_spend_unavailable"
}

// ObserveUsage returns a ctx under which the usage the column LLM client reports (via
// UsageTap) is collected, and a reader of the collected micro-USD and call count. The
// workbook's batch pre-pass uses it to settle its batch attempt.
func ObserveUsage(ctx context.Context) (context.Context, func() (microUSD float64, calls int)) {
	s := &usageSink{}
	return context.WithValue(ctx, usageSinkKey{}, s), func() (float64, int) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.micro, s.n
	}
}

// EstimateBatchMicroUSD is the hold of a batch: every request's estimate at the model price.
func EstimateBatchMicroUSD(reqs []llm.BatchRequest, model string) int64 {
	var total int64
	for _, r := range reqs {
		total += estimateMicroUSD(model, len(r.Prompt)+len(r.System), r.MaxTokens, 1)
	}
	return total
}
