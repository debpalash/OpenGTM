package leadsread

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Count is one group-by bucket. A NULL group key is "null", which is what
// Python's json.dumps writes for a None dict key.
type Count struct {
	Key string
	N   int64
}

// Counts is an ordered bucket list that marshals as a JSON object (Python
// dict) in order.
type Counts []Count

func (c Counts) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	seen := map[string]bool{}
	first := true
	for _, e := range c {
		if seen[e.Key] {
			continue
		}
		seen[e.Key] = true
		if !first {
			b.WriteByte(',')
		}
		first = false
		k, _ := json.Marshal(e.Key)
		b.Write(k)
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(e.N, 10))
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Float1 marshals like a Python float rounded to one decimal: 12 -> 12.0.
type Float1 float64

func (f Float1) MarshalJSON() ([]byte, error) {
	b := strconv.AppendFloat(nil, float64(f), 'f', 1, 64)
	return b, nil
}

// Enrichment mirrors the "enrichment" object of get_stats.
type Enrichment struct {
	Total        int64  `json:"total"`
	WithEmail    int64  `json:"with_email"`
	WithPhone    int64  `json:"with_phone"`
	WithWebsite  int64  `json:"with_website"`
	WithLinkedin int64  `json:"with_linkedin"`
	WithContact  int64  `json:"with_contact"`
	AvgScore     Float1 `json:"avg_score"`
}

// StatsResult mirrors PgLeadStore.get_stats; field order is the Python key order.
type StatsResult struct {
	Total      int64      `json:"total"`
	ByStatus   Counts     `json:"by_status"`
	ByTier     Counts     `json:"by_tier"`
	ByCity     Counts     `json:"by_city"`
	BySource   Counts     `json:"by_source"`
	Enrichment Enrichment `json:"enrichment"`
}

func key(t pgtype.Text) string {
	if !t.Valid {
		return "null"
	}
	return t.String
}

// Stats ports PgLeadStore.get_stats for the workspace bound to tx. by_city is
// the 15 largest cities (ties broken by name, which Python leaves unspecified).
func Stats(ctx context.Context, tx pgx.Tx) (StatsResult, error) {
	q := gen.New(tx)
	t, err := q.LeadStatsTotals(ctx)
	if err != nil {
		return StatsResult{}, err
	}
	res := StatsResult{
		Total:    t.Total,
		ByStatus: Counts{}, ByTier: Counts{}, ByCity: Counts{}, BySource: Counts{},
		Enrichment: Enrichment{
			Total: t.Total, WithEmail: t.WithEmail, WithPhone: t.WithPhone,
			WithWebsite: t.WithWebsite, WithLinkedin: t.WithLinkedin, WithContact: t.WithContact,
		},
	}
	// Python: round(float(avg or 0), 1); FormatFloat rounds the exact binary
	// value half-to-even, as Python's round does.
	avg, _ := strconv.ParseFloat(strconv.FormatFloat(t.AvgScore, 'f', 1, 64), 64)
	res.Enrichment.AvgScore = Float1(avg)

	st, err := q.LeadCountsByStatus(ctx)
	if err != nil {
		return StatsResult{}, err
	}
	for _, r := range st {
		res.ByStatus = append(res.ByStatus, Count{key(r.Key), r.N})
	}
	ti, err := q.LeadCountsByTier(ctx)
	if err != nil {
		return StatsResult{}, err
	}
	for _, r := range ti {
		res.ByTier = append(res.ByTier, Count{key(r.Key), r.N})
	}
	ci, err := q.LeadCountsByCity(ctx)
	if err != nil {
		return StatsResult{}, err
	}
	for _, r := range ci {
		res.ByCity = append(res.ByCity, Count{key(r.Key), r.N})
	}
	so, err := q.LeadCountsBySource(ctx)
	if err != nil {
		return StatsResult{}, err
	}
	for _, r := range so {
		res.BySource = append(res.BySource, Count{key(r.Key), r.N})
	}
	return res, nil
}

func strs(ts []pgtype.Text) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.String)
	}
	return out
}

// FilterOptions ports PgLeadStore.get_filter_options: distinct non-empty facet
// values (specializations capped at 50) and the workspace lead total.
func FilterOptions(ctx context.Context, tx pgx.Tx) (map[string]any, error) {
	q := gen.New(tx)
	cities, err := q.LeadDistinctCities(ctx)
	if err != nil {
		return nil, err
	}
	tiers, err := q.LeadDistinctTiers(ctx)
	if err != nil {
		return nil, err
	}
	sources, err := q.LeadDistinctSources(ctx)
	if err != nil {
		return nil, err
	}
	statuses, err := q.LeadDistinctStatuses(ctx)
	if err != nil {
		return nil, err
	}
	specs, err := q.LeadDistinctSpecializations(ctx)
	if err != nil {
		return nil, err
	}
	t, err := q.LeadStatsTotals(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"cities": strs(cities), "tiers": strs(tiers), "sources": strs(sources),
		"statuses": strs(statuses), "specializations": strs(specs), "total_leads": t.Total,
	}, nil
}
