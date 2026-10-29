//go:build integration

package copilot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// Final security M3: the copilot LLM budget is a per-workspace, per-UTC-day cap in the spend ledger's workspace
// scope. Settled amounts (from usage) replace the reservation; another workspace and the next day are separate.
func TestLedgerBudgetDailyCapPerWorkspace(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ctx := context.Background()
	clean := func() {
		for _, w := range []string{"ws-llmb-a", "ws-llmb-b"} {
			_ = tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM workbook_spend_attempts WHERE workspace_id=$1`, w)
				return err
			})
		}
	}
	clean()
	t.Cleanup(clean)
	now := time.Date(2026, 10, 28, 23, 0, 0, 0, time.UTC)
	b := &LedgerBudget{Pool: pool, DailyCapMicroUSD: 1_000_000, Now: func() time.Time { return now }}
	refused := func(err error) bool {
		var be *BudgetExceededError
		return errors.As(err, &be)
	}

	h, err := b.Reserve(ctx, RoundBid{WorkspaceID: "ws-llmb-a", ConvID: "c1", ExposureMicroUSD: 600_000})
	if err != nil {
		t.Fatal(err)
	}
	h.Settle(ctx, RoundCharge{MicroUSD: 700_000, PromptTokens: 10, CompletionTokens: 5})
	if _, err := b.Reserve(ctx, RoundBid{WorkspaceID: "ws-llmb-a", ConvID: "c1", ExposureMicroUSD: 400_000}); !refused(err) {
		t.Fatalf("over cap: %v", err)
	}
	h2, err := b.Reserve(ctx, RoundBid{WorkspaceID: "ws-llmb-a", ConvID: "c2", ExposureMicroUSD: 300_000})
	if err != nil {
		t.Fatalf("within cap: %v", err)
	}
	// An unsettled hold still counts at its reservation.
	if _, err := b.Reserve(ctx, RoundBid{WorkspaceID: "ws-llmb-a", ConvID: "c2", ExposureMicroUSD: 1}); !refused(err) {
		t.Fatalf("outstanding hold not counted: %v", err)
	}
	h2.Settle(ctx, RoundCharge{MicroUSD: 100_000})
	if _, err := b.Reserve(ctx, RoundBid{WorkspaceID: "ws-llmb-a", ConvID: "c2", ExposureMicroUSD: 100_000}); err != nil {
		t.Fatalf("settled below reservation frees budget: %v", err)
	}
	if _, err := b.Reserve(ctx, RoundBid{WorkspaceID: "ws-llmb-b", ConvID: "c1", ExposureMicroUSD: 900_000}); err != nil {
		t.Fatalf("other workspace: %v", err)
	}
	now = now.Add(2 * time.Hour) // next UTC day
	if _, err := b.Reserve(ctx, RoundBid{WorkspaceID: "ws-llmb-a", ConvID: "c3", ExposureMicroUSD: 900_000}); err != nil {
		t.Fatalf("next day: %v", err)
	}
}
