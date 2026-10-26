package settings

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

func row(provider string, calls, tokens, limit, remaining int32, reset string) gen.ListLLMUsageForDateRow {
	return gen.ListLLMUsageForDateRow{Provider: provider, Model: "m", Calls: calls, TotalTokens: tokens,
		RateLimit: limit, RateRemaining: remaining, RateReset: reset}
}

func TestLLMUsageLimitFallbackAndLive(t *testing.T) {
	reg := mustLoad(t)
	v := BuildLLMUsage(reg, []gen.ListLLMUsageForDateRow{
		row("groq", 250, 1000, 0, 0, ""),           // fallback 1000 RPD
		row("cerebras", 80, 5, 0, 0, ""),           // over the fallback: remaining clamps to 0, pct to 100
		row("mystery", 7, 3, 0, 0, ""),             // unknown provider: limit 0, label Unknown
		row("openrouter", 10, 20, 200, 150, "10s"), // live header limit wins; remaining is the header's
	}, gen.LLMUsageTotalsRow{TotalCalls: 999, TotalTokens: 8888})

	by := map[string]LLMUsageProvider{}
	for _, p := range v.Providers {
		by[p.Provider] = p
	}
	if p := by["groq"]; p.DailyLimit != 1000 || p.LimitLabel != "1K RPD" || p.Remaining != 750 || p.Pct != 25 || p.Live || p.RateReset != "" {
		t.Errorf("groq %+v", p)
	}
	if p := by["cerebras"]; p.DailyLimit != 50 || p.Remaining != 0 || p.Pct != 100 {
		t.Errorf("cerebras %+v", p)
	}
	if p := by["mystery"]; p.DailyLimit != 0 || p.LimitLabel != "Unknown" || p.Remaining != 0 || p.Pct != 0 {
		t.Errorf("mystery %+v", p)
	}
	if p := by["openrouter"]; p.DailyLimit != 200 || p.LimitLabel != "200 RPD (live)" || p.Remaining != 150 || p.Pct != 5 || !p.Live || p.RateReset != "10s" {
		t.Errorf("openrouter %+v", p)
	}
	if v.TodayCalls != 347 || v.TodayTokens != 1028 || v.AllTimeCalls != 999 || v.AllTimeTokens != 8888 {
		t.Errorf("totals %+v", v)
	}
	if v.Providers[0].Provider != "groq" {
		t.Error("row order must be preserved")
	}
}

func TestLLMUsagePctBankersRounding(t *testing.T) {
	reg := mustLoad(t)
	// groq limit 1000 -> use cerebras (50) and other limits: 1/8 -> 12.5 -> 12 (round half to even), 1/24 -> 4.17 -> 4.
	cases := []struct {
		calls, limit int32
		want         int
	}{{1, 8, 12}, {1, 24, 4}, {3, 8, 38}, {5, 8, 62}, {1, 200, 0}, {3, 200, 2}, {5, 200, 2}}
	for _, c := range cases {
		v := BuildLLMUsage(reg, []gen.ListLLMUsageForDateRow{row("x", c.calls, 0, c.limit, 0, "")}, gen.LLMUsageTotalsRow{})
		if got := v.Providers[0].Pct; got != c.want {
			t.Errorf("%d/%d -> %d, want %d", c.calls, c.limit, got, c.want)
		}
	}
}

func TestLLMUsageEmptyShape(t *testing.T) {
	v := BuildLLMUsage(mustLoad(t), nil, gen.LLMUsageTotalsRow{})
	if v.Providers == nil || len(v.Providers) != 0 {
		t.Fatal("providers must be a non-nil empty slice (JSON [])")
	}
}
