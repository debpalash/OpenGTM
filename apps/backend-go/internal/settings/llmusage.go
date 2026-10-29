package settings

import (
	"context"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// LLMUsageProvider is one provider entry of GET /api/settings/llm-usage.
type LLMUsageProvider struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Calls      int    `json:"calls"`
	Tokens     int    `json:"tokens"`
	DailyLimit int    `json:"daily_limit"`
	LimitLabel string `json:"limit_label"`
	Remaining  int    `json:"remaining"`
	Pct        int    `json:"pct"`
	RateReset  string `json:"rate_reset"`
	Live       bool   `json:"live"`
}

// LLMUsageView is the body of GET /api/settings/llm-usage.
type LLMUsageView struct {
	Providers     []LLMUsageProvider `json:"providers"`
	TodayCalls    int64              `json:"today_calls"`
	TodayTokens   int64              `json:"today_tokens"`
	AllTimeCalls  int64              `json:"all_time_calls"`
	AllTimeTokens int64              `json:"all_time_tokens"`
}

// LLMUsage reads llm_usage_daily (tenant table, FORCE RLS) for a workspace.
type LLMUsage struct {
	Reg  *Registry
	Pool *pgxpool.Pool
	Now  func() time.Time // default time.Now
}

// Get returns the usage of workspaceID for date ("" = today, UTC).
func (u *LLMUsage) Get(ctx context.Context, workspaceID, date string) (LLMUsageView, error) {
	if date == "" {
		now := time.Now
		if u.Now != nil {
			now = u.Now
		}
		date = now().UTC().Format("2006-01-02")
	}
	var rows []gen.ListLLMUsageForDateRow
	var tot gen.LLMUsageTotalsRow
	err := tenancy.InWorkspace(ctx, u.Pool, workspaceID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		var err error
		if rows, err = q.ListLLMUsageForDate(ctx, gen.ListLLMUsageForDateParams{WorkspaceID: workspaceID, Date: date}); err != nil {
			return err
		}
		tot, err = q.LLMUsageTotals(ctx, workspaceID)
		return err
	})
	if err != nil {
		return LLMUsageView{}, err
	}
	return BuildLLMUsage(u.Reg, rows, tot), nil
}

// BuildLLMUsage applies the DAILY_LIMITS fallback and live-header logic of
// routers/settings.py get_llm_usage.
func BuildLLMUsage(reg *Registry, rows []gen.ListLLMUsageForDateRow, tot gen.LLMUsageTotalsRow) LLMUsageView {
	v := LLMUsageView{Providers: make([]LLMUsageProvider, 0, len(rows)), AllTimeCalls: tot.TotalCalls, AllTimeTokens: tot.TotalTokens}
	for _, r := range rows {
		fallback, ok := reg.DailyLimits.Get(r.Provider)
		if !ok {
			fallback = DailyLimit{Calls: 0, Label: "Unknown"}
		}
		limit, remaining, label := int(fallback.Calls), 0, fallback.Label
		live := r.RateLimit > 0
		if live {
			limit, remaining, label = int(r.RateLimit), int(r.RateRemaining), strconv.Itoa(int(r.RateLimit))+" RPD (live)"
		} else {
			remaining = max(0, limit-int(r.Calls))
		}
		pct := 0
		if limit > 0 {
			// Python round() is banker's rounding.
			pct = min(100, int(math.RoundToEven(float64(r.Calls)/float64(limit)*100)))
		}
		v.Providers = append(v.Providers, LLMUsageProvider{
			Provider: r.Provider, Model: r.Model, Calls: int(r.Calls), Tokens: int(r.TotalTokens),
			DailyLimit: limit, LimitLabel: label, Remaining: remaining, Pct: pct, RateReset: r.RateReset, Live: live,
		})
		v.TodayCalls += int64(r.Calls)
		v.TodayTokens += int64(r.TotalTokens)
	}
	return v
}
