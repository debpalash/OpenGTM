//go:build integration

package api

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/copilot"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

const chatHi = `{"messages":[{"role":"user","content":"hi"}]}`

func chatWant429(t *testing.T, e *chatEnv, who, ws, what string) {
	t.Helper()
	before := e.convCount(t, ws)
	rec := e.post(who, ws, chatHi)
	if rec.Code != 429 {
		t.Fatalf("%s: want 429, got %d %q", what, rec.Code, rec.Body.String())
	}
	if s, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || s < 1 {
		t.Fatalf("%s: Retry-After %q", what, rec.Header().Get("Retry-After"))
	}
	if e.convCount(t, ws) != before {
		t.Fatalf("%s: a throttled turn created a conversation", what)
	}
}

// Final security M3: copilot turns are rate limited per user.
func TestChatRateLimitedPerUser(t *testing.T) {
	e := newChatEnv(t, chatOpts{limiter: frozenLimiter(), perUserRate: 2, perWSRate: 100})
	for i := 0; i < 2; i++ {
		if rec := e.post("alice", "ws-alpha", chatHi); rec.Code != 200 {
			t.Fatalf("turn %d: %d %q", i, rec.Code, rec.Body.String())
		}
	}
	chatWant429(t, e, "alice", "ws-alpha", "per-user")
}

// Final security M3: and per workspace, whoever sends them.
func TestChatRateLimitedPerWorkspace(t *testing.T) {
	e := newChatEnv(t, chatOpts{limiter: frozenLimiter(), perUserRate: 100, perWSRate: 1})
	if rec := e.post("alice", "ws-alpha", chatHi); rec.Code != 200 {
		t.Fatalf("first: %d", rec.Code)
	}
	chatWant429(t, e, "alice", "ws-alpha", "per-workspace")
	if rec := e.post("bob", "ws-beta", chatHi); rec.Code != 200 {
		t.Fatalf("other workspace: %d %q", rec.Code, rec.Body.String())
	}
}

// Final security M3: an exhausted daily LLM budget ends the turn with a clear error frame before any provider call.
func TestChatLLMBudgetExhaustedErrorFrame(t *testing.T) {
	e := newChatEnv(t, chatOpts{budgetUSD: 0.000001})
	ctx := context.Background()
	clean := func() {
		_ = tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM workbook_spend_attempts WHERE workspace_id='ws-alpha' AND column_id='copilot_llm'`)
			return err
		})
	}
	clean()
	t.Cleanup(clean)
	rec := e.post("alice", "ws-alpha", chatHi)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `{"error": "`+copilot.ErrTextLLMBudget+`"}`) {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if n := len(e.fake.llmReqs); n != 0 {
		t.Fatalf("%d provider rounds ran", n)
	}
}
