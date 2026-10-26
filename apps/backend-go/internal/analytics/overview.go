// Package analytics ports the Postgres-lead-store branch of routers/analytics.py.
// The jobs/llm trend sections are file-only in Python and degrade to empty
// shapes on the PG store; Go reproduces those shapes (deliberate, see the
// contract README).
package analytics

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
)

// Pct is Python's `round(n / total * 100, 1) if total else 0`: a one-decimal
// float (round-half-even on the exact binary value, as Python), or the int 0.
func Pct(n, total int64) any {
	if total == 0 {
		return 0
	}
	v := float64(n) / float64(total) * 100
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', 1, 64), 64)
	return leadsread.Float1(r)
}

// EnrichmentCoverage is the "enrichment" object of the overview (Python key order).
type EnrichmentCoverage struct {
	Total        int64 `json:"total"`
	WithEmail    int64 `json:"with_email"`
	WithPhone    int64 `json:"with_phone"`
	WithWebsite  int64 `json:"with_website"`
	WithContact  int64 `json:"with_contact"`
	WithLinkedin int64 `json:"with_linkedin"`
	EmailPct     any   `json:"email_pct"`
	PhonePct     any   `json:"phone_pct"`
	WebsitePct   any   `json:"website_pct"`
	ContactPct   any   `json:"contact_pct"`
}

type Jobs struct {
	Total       int `json:"total"`
	Completed   int `json:"completed"`
	Failed      int `json:"failed"`
	SuccessRate int `json:"success_rate"`
}

type OverviewResult struct {
	TotalLeads      int64              `json:"total_leads"`
	LeadsThisWeek   int                `json:"leads_this_week"`
	LeadsThisMonth  int                `json:"leads_this_month"`
	AvgScore        leadsread.Float1   `json:"avg_score"`
	Tiers           leadsread.Counts   `json:"tiers"`
	Enrichment      EnrichmentCoverage `json:"enrichment"`
	EmailConfidence map[string]int     `json:"email_confidence"`
	Jobs            Jobs               `json:"jobs"`
}

// Overview serves GET /api/analytics/overview for the workspace bound to tx.
func Overview(ctx context.Context, tx pgx.Tx) (OverviewResult, error) {
	s, err := leadsread.Stats(ctx, tx)
	if err != nil {
		return OverviewResult{}, err
	}
	e, t := s.Enrichment, s.Total
	return OverviewResult{
		TotalLeads: t,
		AvgScore:   e.AvgScore,
		Tiers:      s.ByTier,
		Enrichment: EnrichmentCoverage{
			Total: t, WithEmail: e.WithEmail, WithPhone: e.WithPhone, WithWebsite: e.WithWebsite,
			WithContact: e.WithContact, WithLinkedin: e.WithLinkedin,
			EmailPct: Pct(e.WithEmail, t), PhonePct: Pct(e.WithPhone, t),
			WebsitePct: Pct(e.WithWebsite, t), ContactPct: Pct(e.WithContact, t),
		},
		EmailConfidence: map[string]int{},
	}, nil
}

type PipelineResult struct {
	Statuses    leadsread.Counts `json:"statuses"`
	StatusTiers map[string]int   `json:"status_tiers"`
}

// Pipeline serves GET /api/analytics/pipeline.
func Pipeline(ctx context.Context, tx pgx.Tx) (PipelineResult, error) {
	s, err := leadsread.Stats(ctx, tx)
	if err != nil {
		return PipelineResult{}, err
	}
	return PipelineResult{Statuses: s.ByStatus, StatusTiers: map[string]int{}}, nil
}

type CityRow struct {
	City     *string `json:"city"`
	Count    int64   `json:"count"`
	AvgScore *int    `json:"avg_score"`
}

type SourceRow struct {
	Source      string `json:"source"`
	Count       int64  `json:"count"`
	AvgScore    *int   `json:"avg_score"`
	WithEmail   *int   `json:"with_email"`
	WithPhone   *int   `json:"with_phone"`
	WithContact *int   `json:"with_contact"`
}

type EnrichmentResult struct {
	SourceQuality     []SourceRow `json:"source_quality"`
	ByCity            []CityRow   `json:"by_city"`
	ScoreDistribution []struct{}  `json:"score_distribution"`
}

// Enrichment serves GET /api/analytics/enrichment. The score/email aggregates
// are null on the PG store (Python only has counts there).
func Enrichment(ctx context.Context, tx pgx.Tx) (EnrichmentResult, error) {
	s, err := leadsread.Stats(ctx, tx)
	if err != nil {
		return EnrichmentResult{}, err
	}
	res := EnrichmentResult{SourceQuality: []SourceRow{}, ByCity: []CityRow{}, ScoreDistribution: []struct{}{}}
	for _, c := range s.ByCity {
		row := CityRow{Count: c.N}
		if !c.Null {
			k := c.Key
			row.City = &k
		}
		res.ByCity = append(res.ByCity, row)
	}
	for _, c := range s.BySource {
		src := c.Key
		if c.Null || src == "" { // Python: `src or "unknown"`
			src = "unknown"
		}
		res.SourceQuality = append(res.SourceQuality, SourceRow{Source: src, Count: c.N})
	}
	return res, nil
}

// Collection is the file-only jobs/leads trend, empty on the PG store.
type CollectionResult struct {
	JobsByDay      []struct{} `json:"jobs_by_day"`
	LeadsByDay     []struct{} `json:"leads_by_day"`
	AvgLeadsPerJob int        `json:"avg_leads_per_job"`
}

func Collection() CollectionResult {
	return CollectionResult{JobsByDay: []struct{}{}, LeadsByDay: []struct{}{}}
}

type LLMResult struct {
	ByDay       []struct{} `json:"by_day"`
	ByProvider  []struct{} `json:"by_provider"`
	TotalTokens int        `json:"total_tokens"`
	TotalCalls  int        `json:"total_calls"`
}

func LLM() LLMResult { return LLMResult{ByDay: []struct{}{}, ByProvider: []struct{}{}} }
