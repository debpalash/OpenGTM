package signals

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Result types carry Python's key order (dict insertion order).
type Summary struct {
	Total          int64   `json:"total"`
	WeightedScore  int64   `json:"weighted_score"`
	ActiveAccounts int64   `json:"active_accounts"`
	MomentumPct    *Float1 `json:"momentum_pct"`
}

type TrendPoint struct {
	Date   string `json:"date"`
	Count  int64  `json:"count"`
	Weight int64  `json:"weight"`
}

type TypeStat struct {
	SignalType string `json:"signal_type"`
	Count      int64  `json:"count"`
	Weight     int64  `json:"weight"`
}

type SourceStat struct {
	Source string `json:"source"`
	Count  int64  `json:"count"`
}

type AccountStat struct {
	LeadID  *int64 `json:"lead_id"`
	Company string `json:"company"`
	Count   int64  `json:"count"`
	Weight  int64  `json:"weight"`
}

type AnalyticsResult struct {
	PeriodDays  int           `json:"period_days"`
	Summary     Summary       `json:"summary"`
	Trend       []TrendPoint  `json:"trend"`
	ByType      []TypeStat    `json:"by_type"`
	BySource    []SourceStat  `json:"by_source"`
	TopAccounts []AccountStat `json:"top_accounts"`
}

// Float1 renders like a Python float already rounded to one decimal (150 -> 150.0).
type Float1 float64

func (f Float1) MarshalJSON() ([]byte, error) {
	return strconv.AppendFloat(nil, float64(f), 'f', 1, 64), nil
}

// Momentum is round((total - previous) / previous * 100, 1); nil when previous
// is 0. strconv rounds the exact binary value half-to-even, like Python's round.
func Momentum(total, previous int64) *float64 {
	if previous == 0 {
		return nil
	}
	v := float64(total-previous) / float64(previous) * 100
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', 1, 64), 64)
	return &r
}

// Analytics ports the /api/signals/analytics aggregation for a window of days
// ending at now. The day buckets keep CAST(created_at / 86400 AS INTEGER)
// verbatim: Postgres ROUNDS that cast (Python/Postgres parity quirk), so a
// signal after 12:00 UTC is counted in the next day's bucket; a bucket past
// today is simply absent from the trend. Run inside tenancy.InWorkspace.
func Analytics(ctx context.Context, tx pgx.Tx, days int, now time.Time) (AnalyticsResult, error) {
	q := gen.New(tx)
	nowF := float64(now.UnixMicro()) / 1e6
	cutoff := nowF - float64(days)*86400

	sum, err := q.SignalAnalyticsSummary(ctx, cutoff)
	if err != nil {
		return AnalyticsResult{}, err
	}
	res := AnalyticsResult{
		PeriodDays: days,
		Trend:      []TrendPoint{}, ByType: []TypeStat{}, BySource: []SourceStat{}, TopAccounts: []AccountStat{},
	}
	res.Summary = Summary{Total: sum.Total, WeightedScore: sum.Weighted, ActiveAccounts: sum.Accounts}

	types, err := q.SignalAnalyticsByType(ctx, cutoff)
	if err != nil {
		return AnalyticsResult{}, err
	}
	for _, r := range types {
		name := "unknown"
		if r.SignalType.Valid && r.SignalType.String != "" {
			name = r.SignalType.String
		}
		res.ByType = append(res.ByType, TypeStat{SignalType: name, Count: r.N, Weight: r.Weight})
	}
	srcs, err := q.SignalAnalyticsBySource(ctx, cutoff)
	if err != nil {
		return AnalyticsResult{}, err
	}
	for _, r := range srcs {
		name := "unknown"
		if r.Source.Valid && r.Source.String != "" {
			name = r.Source.String
		}
		res.BySource = append(res.BySource, SourceStat{Source: name, Count: r.N})
	}
	accts, err := q.SignalAnalyticsTopAccounts(ctx, cutoff)
	if err != nil {
		return AnalyticsResult{}, err
	}
	for _, r := range accts {
		lead := i64p(r.LeadID)
		company := ""
		if r.Company.Valid {
			company = r.Company.String
		}
		if company == "" {
			if lead == nil {
				company = "Lead None"
			} else {
				company = "Lead " + strconv.FormatInt(*lead, 10)
			}
		}
		res.TopAccounts = append(res.TopAccounts, AccountStat{LeadID: lead, Company: company, Count: r.N, Weight: r.Weight})
	}

	bucketRows, err := q.SignalAnalyticsDayBuckets(ctx, cutoff)
	if err != nil {
		return AnalyticsResult{}, err
	}
	buckets := make(map[int64]gen.SignalAnalyticsDayBucketsRow, len(bucketRows))
	for _, r := range bucketRows {
		buckets[int64(r.Day)] = r
	}
	today := now.Unix() / 86400
	if now.Unix() < 0 && now.Unix()%86400 != 0 {
		today-- // floor division like Python's //
	}
	for d := today - int64(days) + 1; d <= today; d++ {
		p := TrendPoint{Date: time.Unix(d*86400, 0).UTC().Format("2006-01-02")}
		if b, ok := buckets[d]; ok {
			p.Count, p.Weight = b.N, b.Weight
		}
		res.Trend = append(res.Trend, p)
	}

	prev, err := q.SignalAnalyticsPrevious(ctx, gen.SignalAnalyticsPreviousParams{
		PreviousCutoff: cutoff - float64(days)*86400, Cutoff: cutoff,
	})
	if err != nil {
		return AnalyticsResult{}, err
	}
	if m := Momentum(sum.Total, prev); m != nil {
		f := Float1(*m)
		res.Summary.MomentumPct = &f
	}
	return res, nil
}
