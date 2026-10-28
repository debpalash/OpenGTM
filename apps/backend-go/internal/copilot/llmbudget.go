package copilot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autopilot"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/spend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// The copilot LLM budget (final security M3). Every llm round of a chat turn is an attempt of the spend ledger's
// workspace scope (spend.WorkspaceLedger): reserved at an estimate before the provider is called, dispatched, and
// settled after the round from its usage. The ledger cell is the workspace's UTC day, so the cell limit is the
// workspace's daily LLM budget (COPILOT_LLM_DAILY_BUDGET_USD). Python had no budget for chat.

// ErrTextLLMBudget is the error frame of a turn whose round the daily budget refused.
const ErrTextLLMBudget = "This workspace has reached its daily AI usage budget. Try again tomorrow, or ask an admin to raise COPILOT_LLM_DAILY_BUDGET_USD."

// RoundBudget admits llm rounds. A refusal is *BudgetExceededError; any other error fails the round closed.
type RoundBudget interface {
	Reserve(ctx context.Context, bid RoundBid) (RoundHold, error)
}

// RoundBid is one round's admission request.
type RoundBid struct {
	WorkspaceID, ConvID string
	ExposureMicroUSD    int64 // the round's worst-case cost estimate
}

// RoundHold is an admitted round; Settle is called once when the round ends, however it ends.
type RoundHold interface {
	Settle(ctx context.Context, c RoundCharge)
}

// RoundCharge is what a round cost: from the provider's token usage, or estimated from the bytes in and out when
// the provider reported none (the streaming chat path).
type RoundCharge struct {
	MicroUSD                       int64
	PromptTokens, CompletionTokens int
	Estimated                      bool
}

// BudgetExceededError is a round the budget refused.
type BudgetExceededError struct{ Reason string }

func (e *BudgetExceededError) Error() string { return "copilot llm budget refused: " + e.Reason }

// roundMaxOutputTokens bounds one round's completion for the estimate (the Anthropic turn's max_tokens; the
// OpenAI path asks for less).
const roundMaxOutputTokens = anthropicTokens

// bytesPerToken is the conservative bytes-per-token ratio of the estimates (real text averages about 4).
const bytesPerToken = 3

func tokensFor(n int) int { return (n + bytesPerToken - 1) / bytesPerToken }

// usdMicro prices tokens at per-million-token USD rates, rounded up to whole micro-USD.
func usdMicro(prompt, completion int, in, out float64) int64 {
	v := math.Ceil(float64(prompt)*in + float64(completion)*out) // per-MTok rate * tokens = micro-USD
	if v > math.MaxInt64/2 {
		return math.MaxInt64 / 2
	}
	return int64(v)
}

// roundModel is the model of the round's provider (cursor into the chain); "" when unknown, which prices at the
// most expensive tier.
func (t *Turn) roundModel(ctx context.Context, cursor int) string {
	if t.Engine == nil || t.Engine.LLM == nil {
		return ""
	}
	chain := t.Engine.chatChain(ctx)
	if len(chain) == 0 {
		return ""
	}
	return chain[min(max(cursor, 0), len(chain)-1)].Model
}

func (t *Turn) usageNow() llm.TokenUsage {
	if t.Engine == nil || t.Engine.LLM == nil {
		return llm.TokenUsage{}
	}
	return t.Engine.LLM.Usage()
}

// meteredRound reserves the round, opens it, and wraps the source so its end settles the hold.
func (t *Turn) meteredRound(ctx context.Context, rq RoundRequest) (FrameSource, error) {
	if t.Budget == nil {
		return t.openRound(ctx, rq)
	}
	msgs, err := json.Marshal(rq.Messages)
	if err != nil {
		return nil, err
	}
	in, out := llm.PriceForModel(t.roundModel(ctx, rq.Cursor))
	promptEst := tokensFor(len(msgs))
	hold, err := t.Budget.Reserve(ctx, RoundBid{WorkspaceID: t.WS, ConvID: t.ConvID,
		ExposureMicroUSD: usdMicro(promptEst, roundMaxOutputTokens, in, out)})
	if err != nil {
		var be *BudgetExceededError
		if errors.As(err, &be) {
			return nil, &turnErr{ErrTextLLMBudget, err}
		}
		return nil, err
	}
	before := t.usageNow()
	src, err := t.openRound(ctx, rq)
	m := &meteredSrc{src: src, hold: hold, ctx: ctx, in: in, out: out, promptEst: promptEst, before: before, usage: t.usageNow}
	if err != nil {
		m.Close()
		return nil, err
	}
	return m, nil
}

// meteredSrc counts the bytes a round delivers and settles the hold on Close.
type meteredSrc struct {
	src       FrameSource
	hold      RoundHold
	ctx       context.Context
	in, out   float64
	promptEst int
	outBytes  int
	before    llm.TokenUsage
	usage     func() llm.TokenUsage
	settled   bool
}

func (m *meteredSrc) Next() (Frame, error) {
	f, err := m.src.Next()
	if err == nil {
		m.outBytes += len(f.D)
	}
	return f, err
}

// Close settles once: from the client's usage delta when the provider reported tokens, else from the estimate.
func (m *meteredSrc) Close() {
	if m.src != nil {
		m.src.Close()
	}
	if m.settled {
		return
	}
	m.settled = true
	after := m.usage()
	prompt, completion := after.PromptTokens-m.before.PromptTokens, after.CompletionTokens-m.before.CompletionTokens
	c := RoundCharge{PromptTokens: prompt, CompletionTokens: completion}
	if prompt <= 0 && completion <= 0 {
		c = RoundCharge{PromptTokens: m.promptEst, CompletionTokens: tokensFor(m.outBytes), Estimated: true}
	}
	c.MicroUSD = usdMicro(c.PromptTokens, c.CompletionTokens, m.in, m.out)
	m.hold.Settle(m.ctx, c)
}

// LedgerBudget is the Postgres RoundBudget: the spend ledger's workspace scope, one cell per workspace and UTC day.
type LedgerBudget struct {
	Pool             *pgxpool.Pool
	DailyCapMicroUSD int64
	Now              func() time.Time // default time.Now
}

const (
	budgetColumn  = "copilot_llm"
	budgetRow     = "workspace"
	budgetTimeout = 10 * time.Second
)

func (l *LedgerBudget) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *LedgerBudget) Reserve(ctx context.Context, bid RoundBid) (RoundHold, error) {
	if l.Pool == nil || bid.WorkspaceID == "" {
		return nil, errors.New("copilot: llm budget without a pool or workspace")
	}
	run := "copilot_llm:" + l.now().UTC().Format("2006-01-02")
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	key, err := canonjson.Marshal([]any{bid.WorkspaceID, spend.WorkspaceLedger, run, budgetRow, budgetColumn, hex.EncodeToString(nonce)}, false)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(key)
	r := spend.Reservation{WorkspaceID: bid.WorkspaceID, WorkbookID: spend.WorkspaceLedger, RunID: run, RowIdentity: budgetRow,
		ColumnID: budgetColumn, Provider: "llm", AttemptKey: hex.EncodeToString(sum[:]),
		ExposureMicroUSD: max(bid.ExposureMicroUSD, 0), CellLimitMicroUSD: l.DailyCapMicroUSD,
		CostBasis: map[string]any{"kind": "catalog_estimate", "provider": "llm"},
		Operation: map[string]any{"kind": "copilot_round", "conversation_id": bid.ConvID}}
	var adm spend.Admission
	var dispatched bool
	if err := tenancy.InWorkspace(ctx, l.Pool, bid.WorkspaceID, func(tx pgx.Tx) error {
		var e error
		if adm, e = spend.ReserveWorkspace(ctx, tx, r); e != nil || !adm.OK {
			return e
		}
		// The provider call follows at once: dispatch in the same transaction.
		dispatched, e = spend.Transition(ctx, tx, spend.Identity{WorkspaceID: bid.WorkspaceID, WorkbookID: spend.WorkspaceLedger,
			AttemptID: adm.ID, ContractHash: adm.ContractHash}, "dispatch")
		return e
	}); err != nil {
		return nil, err
	}
	if !adm.OK {
		return nil, &BudgetExceededError{Reason: adm.Reason}
	}
	if !dispatched {
		return nil, &BudgetExceededError{Reason: "attempt_not_dispatchable"}
	}
	return &ledgerRoundHold{pool: l.Pool, id: spend.Identity{WorkspaceID: bid.WorkspaceID, WorkbookID: spend.WorkspaceLedger,
		AttemptID: adm.ID, ContractHash: adm.ContractHash}}, nil
}

type ledgerRoundHold struct {
	pool *pgxpool.Pool
	id   spend.Identity
}

// Settle records the charge; a failed settlement leaves the attempt uncertain, which keeps counting at its
// reservation (never under-counts).
func (h *ledgerRoundHold) Settle(ctx context.Context, c RoundCharge) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budgetTimeout)
	defer cancel()
	var res spend.SettleResult
	err := tenancy.InWorkspace(dctx, h.pool, h.id.WorkspaceID, func(tx pgx.Tx) error {
		var e error
		res, e = spend.SettleWorkspace(dctx, tx, h.id, max(c.MicroUSD, 0), "catalog_estimate", map[string]any{
			"prompt_tokens": int64(c.PromptTokens), "completion_tokens": int64(c.CompletionTokens), "estimated": c.Estimated})
		return e
	})
	if err != nil || !res.OK {
		_ = tenancy.InWorkspace(dctx, h.pool, h.id.WorkspaceID, func(tx pgx.Tx) error {
			_, e := spend.Transition(dctx, tx, h.id, "mark_uncertain")
			return e
		})
	}
}

// meteredLLM puts the non-chat llm call (the autopilot planner behind draft_plan) under the same workspace
// budget as the chat rounds (final security M3 follow-up): the call is reserved before the provider is
// dialled and settled from the reported usage, however it ends.
type meteredLLM struct {
	inner  autopilot.LLM
	budget RoundBudget
	ws     string
	conv   string
}

func (m *meteredLLM) AnthropicProvider(ctx context.Context) (llm.Provider, bool) {
	return m.inner.AnthropicProvider(ctx)
}

func (m *meteredLLM) AnthropicTurn(ctx context.Context, r llm.AnthropicRequest) (*llm.AnthropicResponse, error) {
	model := ""
	if p, ok := m.inner.AnthropicProvider(ctx); ok {
		model = p.Model
	}
	in, out := llm.PriceForModel(model)
	body, err := json.Marshal([]any{r.System, r.Messages})
	if err != nil {
		return nil, err
	}
	promptEst := tokensFor(len(body))
	maxOut := r.MaxTokens
	if maxOut <= 0 {
		maxOut = roundMaxOutputTokens
	}
	hold, err := m.budget.Reserve(ctx, RoundBid{WorkspaceID: m.ws, ConvID: m.conv, ExposureMicroUSD: usdMicro(promptEst, maxOut, in, out)})
	if err != nil {
		return nil, err
	}
	resp, callErr := m.inner.AnthropicTurn(ctx, r)
	c := RoundCharge{PromptTokens: promptEst, CompletionTokens: maxOut, Estimated: true} // a failed call may have run: charge the bid
	if callErr == nil && resp != nil && (resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0) {
		u := resp.Usage
		c = RoundCharge{PromptTokens: u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens, CompletionTokens: u.OutputTokens}
	} else if callErr == nil && resp != nil {
		c = RoundCharge{PromptTokens: promptEst, CompletionTokens: tokensFor(len(resp.Text())), Estimated: true}
	}
	c.MicroUSD = usdMicro(c.PromptTokens, c.CompletionTokens, in, out)
	hold.Settle(ctx, c)
	return resp, callErr
}
