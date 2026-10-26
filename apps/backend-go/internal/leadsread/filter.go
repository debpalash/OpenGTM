// Package leadsread is the read-only port of PgLeadStore (store.py): filtered
// lead pages, lookups by id, stats and filter facets. It never writes.
package leadsread

import (
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Filter mirrors FilterCriteria (workbook/schemas.py) plus the lead_ids key
// query_leads_page also honours. String filters treat "" as unset (Python
// truthiness); pointer filters treat nil as unset. LeadIDs nil is unset while
// a non-nil (even empty) slice filters to those ids, like `is not None`.
type Filter struct {
	LeadIDs        []int64  `json:"lead_ids,omitempty"`
	City           string   `json:"city,omitempty"`
	State          string   `json:"state,omitempty"`
	ScoreTier      string   `json:"score_tier,omitempty"`
	Status         string   `json:"status,omitempty"`
	Source         string   `json:"source,omitempty"`
	JobIDs         []string `json:"job_ids,omitempty"`
	Specialization string   `json:"specialization,omitempty"`
	CompanySize    string   `json:"company_size,omitempty"`
	HasEmail       *bool    `json:"has_email,omitempty"`
	HasPhone       *bool    `json:"has_phone,omitempty"`
	HasWebsite     *bool    `json:"has_website,omitempty"`
	MinScore       *int     `json:"min_score,omitempty"`
	MaxScore       *int     `json:"max_score,omitempty"`
	Search         string   `json:"search,omitempty"`
}

func ptxt(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func pbool(b *bool) pgtype.Bool {
	if b == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *b, Valid: true}
}

func pint(n *int) pgtype.Int4 {
	if n == nil {
		return pgtype.Int4{}
	}
	// Scores are int4 columns: clamp so an out-of-range bound keeps its meaning.
	v := *n
	switch {
	case v > 1<<31-1:
		v = 1<<31 - 1
	case v < -1<<31:
		v = -1 << 31
	}
	return pgtype.Int4{Int32: int32(v), Valid: true}
}

func toInt32(ids []int64) []int32 {
	out := make([]int32, 0, len(ids))
	for _, id := range ids {
		if id >= -1<<31 && id <= 1<<31-1 { // ids outside int4 cannot exist
			out = append(out, int32(id))
		}
	}
	return out
}

// params builds the shared WHERE arguments for the count and page queries.
func (f Filter) params() (gen.CountLeadsFilteredParams, gen.QueryLeadsPageParams) {
	var sources []string
	for _, j := range f.JobIDs {
		sources = append(sources, "job:"+j)
	}
	c := gen.CountLeadsFilteredParams{
		HasLeadIds:     f.LeadIDs != nil,
		LeadIds:        toInt32(f.LeadIDs),
		City:           ptxt(f.City),
		State:          ptxt(f.State),
		ScoreTier:      ptxt(f.ScoreTier),
		Status:         ptxt(f.Status),
		Source:         ptxt(f.Source),
		CompanySize:    ptxt(f.CompanySize),
		HasJobIds:      len(f.JobIDs) > 0,
		JobIds:         f.JobIDs,
		JobSources:     sources,
		Specialization: ptxt(f.Specialization),
		HasEmail:       pbool(f.HasEmail),
		HasPhone:       pbool(f.HasPhone),
		HasWebsite:     pbool(f.HasWebsite),
		MinScore:       pint(f.MinScore),
		MaxScore:       pint(f.MaxScore),
		Search:         ptxt(f.Search),
	}
	return c, gen.QueryLeadsPageParams{
		HasLeadIds: c.HasLeadIds, LeadIds: c.LeadIds, City: c.City, State: c.State,
		ScoreTier: c.ScoreTier, Status: c.Status, Source: c.Source, CompanySize: c.CompanySize,
		HasJobIds: c.HasJobIds, JobIds: c.JobIds, JobSources: c.JobSources,
		Specialization: c.Specialization, HasEmail: c.HasEmail, HasPhone: c.HasPhone,
		HasWebsite: c.HasWebsite, MinScore: c.MinScore, MaxScore: c.MaxScore, Search: c.Search,
	}
}
