package llm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// TokenUsage tracks token usage across a pipeline run (leadgen/llm.py TokenUsage).
type TokenUsage struct {
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	TotalTokens      int      `json:"total_tokens"`
	CacheWriteTokens int      `json:"cache_write_tokens"`
	CacheReadTokens  int      `json:"cache_read_tokens"`
	Calls            int      `json:"calls"`
	Provider         string   `json:"provider"`
	Errors           []string `json:"errors"` // last 5
}

// UsageRecord is one provider call as persisted to llm_usage_daily: the
// arguments of record_llm_usage.
type UsageRecord struct {
	Provider         string
	Model            string
	PromptTokens     int
	CompletionTokens int
	RateLimit        int
	RateRemaining    int
	RateReset        string
}

// UsageRecorder persists one call. Failures are logged by the client, never
// returned to callers (usage tracking is best effort, as in Python).
type UsageRecorder interface {
	Record(ctx context.Context, workspaceID string, r UsageRecord) error
}

// PgUsage upserts the per-workspace daily aggregate that
// GET /api/settings/llm-usage reads (llm_usage_daily, FORCE RLS).
type PgUsage struct {
	Pool *pgxpool.Pool
	Now  func() time.Time // default time.Now
}

const upsertUsageSQL = `
INSERT INTO llm_usage_daily
  (workspace_id, provider, model, date, calls, prompt_tokens, completion_tokens, total_tokens,
   rate_limit, rate_remaining, rate_reset, updated_at)
VALUES ($1, $2, $3, $4, 1, $5, $6, $5::int + $6::int, $7::int, $8, $9, $10)
ON CONFLICT (workspace_id, provider, date) DO UPDATE SET
  model = EXCLUDED.model,
  calls = llm_usage_daily.calls + 1,
  prompt_tokens = llm_usage_daily.prompt_tokens + EXCLUDED.prompt_tokens,
  completion_tokens = llm_usage_daily.completion_tokens + EXCLUDED.completion_tokens,
  total_tokens = llm_usage_daily.total_tokens + EXCLUDED.total_tokens,
  rate_limit = CASE WHEN $7::int > 0 THEN EXCLUDED.rate_limit ELSE llm_usage_daily.rate_limit END,
  rate_remaining = CASE WHEN $7::int > 0 THEN EXCLUDED.rate_remaining ELSE llm_usage_daily.rate_remaining END,
  rate_reset = CASE WHEN EXCLUDED.rate_reset <> '' THEN EXCLUDED.rate_reset ELSE llm_usage_daily.rate_reset END,
  updated_at = EXCLUDED.updated_at`

// Record adds one call to today's (UTC) row of the workspace/provider.
func (p *PgUsage) Record(ctx context.Context, workspaceID string, r UsageRecord) error {
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	t := now().UTC()
	return tenancy.InWorkspace(ctx, p.Pool, workspaceID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, upsertUsageSQL, workspaceID, r.Provider, r.Model, t.Format("2006-01-02"),
			int32(r.PromptTokens), int32(r.CompletionTokens), int32(r.RateLimit), int32(r.RateRemaining), r.RateReset,
			t.Format("2006-01-02T15:04:05.000000+00:00"))
		if err != nil {
			return fmt.Errorf("llm: record usage: %w", err)
		}
		return nil
	})
}

// usageTracker is the mutex-guarded TokenUsage of a Client.
type usageTracker struct {
	mu sync.Mutex
	u  TokenUsage
}

func (t *usageTracker) add(prompt, completion int, provider string, cacheWrite, cacheRead int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.u.PromptTokens += prompt
	t.u.CompletionTokens += completion
	t.u.TotalTokens += prompt + completion
	t.u.CacheWriteTokens += cacheWrite
	t.u.CacheReadTokens += cacheRead
	t.u.Calls++
	if provider != "" {
		t.u.Provider = provider
	}
}

func (t *usageTracker) addError(s string) {
	t.mu.Lock()
	t.u.Errors = append(t.u.Errors, s)
	t.mu.Unlock()
}

func (t *usageTracker) snapshot() TokenUsage {
	t.mu.Lock()
	defer t.mu.Unlock()
	u := t.u
	if n := len(u.Errors); n > 5 {
		u.Errors = append([]string(nil), u.Errors[n-5:]...)
	} else {
		u.Errors = append([]string{}, u.Errors...)
	}
	return u
}

func (t *usageTracker) allErrors() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.u.Errors...)
}

func (t *usageTracker) reset() {
	t.mu.Lock()
	t.u = TokenUsage{}
	t.mu.Unlock()
}
