//go:build integration

package llm

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type dbRow struct {
	Model                            string
	Calls, Prompt, Completion, Total int
	RateLimit, RateRemaining         int
	RateReset, Date, UpdatedAt       string
}

func readRow(t *testing.T, p *PgUsage, ws, provider, date string) dbRow {
	t.Helper()
	var r dbRow
	err := tenancy.InWorkspace(context.Background(), p.Pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT model, calls, prompt_tokens, completion_tokens, total_tokens,
			rate_limit, rate_remaining, rate_reset, date, updated_at FROM llm_usage_daily
			WHERE workspace_id=$1 AND provider=$2 AND date=$3`, ws, provider, date).
			Scan(&r.Model, &r.Calls, &r.Prompt, &r.Completion, &r.Total, &r.RateLimit, &r.RateRemaining, &r.RateReset, &r.Date, &r.UpdatedAt)
	})
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	return r
}

// The upsert must behave like store.record_llm_usage: calls and tokens
// accumulate, model follows the last call, rate fields only move when the call
// carried a limit, the reset text only when non-empty.
func TestPgUsageUpsertSemantics(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ctx := context.Background()
	ws := fmt.Sprintf("llm-ws-%d", time.Now().UnixNano())
	other := ws + "-other"
	now := time.Date(2026, 10, 28, 23, 59, 58, 123456000, time.UTC)
	p := &PgUsage{Pool: pool, Now: func() time.Time { return now }}
	t.Cleanup(func() {
		for _, w := range []string{ws, other} {
			_ = tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM llm_usage_daily WHERE workspace_id=$1`, w)
				return err
			})
		}
	})

	rec := func(r UsageRecord) {
		t.Helper()
		if err := p.Record(ctx, ws, r); err != nil {
			t.Fatal(err)
		}
	}
	rec(UsageRecord{Provider: "groq", Model: "m1", PromptTokens: 10, CompletionTokens: 5, RateLimit: 1000, RateRemaining: 999, RateReset: "60s"})
	r := readRow(t, p, ws, "groq", "2026-10-28")
	if r.Calls != 1 || r.Prompt != 10 || r.Completion != 5 || r.Total != 15 || r.RateLimit != 1000 || r.RateRemaining != 999 || r.RateReset != "60s" || r.UpdatedAt != "2026-10-28T23:59:58.123456+00:00" {
		t.Fatalf("first insert %+v", r)
	}
	// No rate headers on the second call: the previous rate fields stay, model follows.
	rec(UsageRecord{Provider: "groq", Model: "m2", PromptTokens: 3, CompletionTokens: 4})
	r = readRow(t, p, ws, "groq", "2026-10-28")
	if r.Calls != 2 || r.Total != 22 || r.Model != "m2" || r.RateLimit != 1000 || r.RateRemaining != 999 || r.RateReset != "60s" {
		t.Fatalf("second %+v", r)
	}
	// Headers again: limit/remaining replaced; an empty reset keeps the old text.
	rec(UsageRecord{Provider: "groq", Model: "m2", PromptTokens: 1, CompletionTokens: 1, RateLimit: 500, RateRemaining: 400})
	r = readRow(t, p, ws, "groq", "2026-10-28")
	if r.Calls != 3 || r.RateLimit != 500 || r.RateRemaining != 400 || r.RateReset != "60s" {
		t.Fatalf("third %+v", r)
	}
	// A new UTC day is a new row.
	now = now.Add(2 * time.Second)
	rec(UsageRecord{Provider: "groq", Model: "m2", PromptTokens: 2, CompletionTokens: 2})
	if r = readRow(t, p, ws, "groq", "2026-10-29"); r.Calls != 1 || r.Total != 4 {
		t.Fatalf("next day %+v", r)
	}
	// Tenant isolation: another workspace never sees or bumps these rows.
	if err := p.Record(ctx, other, UsageRecord{Provider: "groq", Model: "x", PromptTokens: 1}); err != nil {
		t.Fatal(err)
	}
	if r = readRow(t, p, ws, "groq", "2026-10-28"); r.Calls != 3 {
		t.Fatalf("other workspace touched %+v", r)
	}

	// The settings route reads the same table.
	reg, err := settings.Load()
	if err != nil {
		t.Fatal(err)
	}
	view, err := (&settings.LLMUsage{Reg: reg, Pool: pool, Now: func() time.Time { return now.Add(-2 * time.Second) }}).Get(ctx, ws, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Providers) != 1 || view.Providers[0].Provider != "groq" || view.Providers[0].Calls != 3 || view.TodayCalls != 3 || view.AllTimeCalls != 4 {
		t.Fatalf("llm-usage view %+v", view)
	}
}

func TestPgUsageRequiresWorkspace(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	if err := (&PgUsage{Pool: pool}).Record(context.Background(), "", UsageRecord{Provider: "groq"}); err == nil {
		t.Fatal("empty workspace must be refused")
	}
}
