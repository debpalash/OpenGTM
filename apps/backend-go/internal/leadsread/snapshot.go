package leadsread

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// LeadKeys is the key order of every lead row dict: the field order of the
// Python Lead dataclass (Lead.to_dict), with search_tsv and founding_year
// (not Lead fields) omitted. Go maps are unordered; consumers that need the
// Python key order should iterate this slice.
var LeadKeys = []string{
	"id",
	"company",
	"website",
	"email",
	"email_confidence",
	"email_provider",
	"phone",
	"phone_provider",
	"contact_person",
	"contact_title",
	"city",
	"state",
	"address",
	"specialization",
	"company_size",
	"employee_count_exact",
	"description",
	"revenue_range",
	"founded_year",
	"industry_tags",
	"technologies",
	"technographics",
	"funding_stage",
	"company_size_basis",
	"linkedin_url",
	"twitter_url",
	"facebook_url",
	"secondary_emails",
	"secondary_phones",
	"decision_makers",
	"glassdoor_rating",
	"hiring_signals",
	"enrichment_attempts",
	"enrichment_waterfall",
	"field_provenance",
	"source",
	"source_url",
	"collection_job_id",
	"workspace_id",
	"score",
	"score_tier",
	"status",
	"yupcha_value_prop",
	"company_need",
	"notes",
	"created_at",
	"updated_at",
	"last_enriched_at",
}

func txt(t pgtype.Text) any {
	if !t.Valid {
		return nil
	}
	return t.String
}

func i4(n pgtype.Int4) any {
	if !n.Valid {
		return nil
	}
	return int64(n.Int32)
}

// leadRow converts a generated row to the Lead.to_dict shape. NULL columns are
// nil (Python passes DB NULLs through Lead.from_dict unchanged).
func leadRow(r gen.LeadsByIDsRow) map[string]any {
	vals := []any{
		int64(r.ID),
		r.Company,
		txt(r.Website),
		txt(r.Email),
		txt(r.EmailConfidence),
		txt(r.EmailProvider),
		txt(r.Phone),
		txt(r.PhoneProvider),
		txt(r.ContactPerson),
		txt(r.ContactTitle),
		txt(r.City),
		txt(r.State),
		txt(r.Address),
		txt(r.Specialization),
		txt(r.CompanySize),
		i4(r.EmployeeCountExact),
		txt(r.Description),
		txt(r.RevenueRange),
		txt(r.FoundedYear),
		txt(r.IndustryTags),
		txt(r.Technologies),
		txt(r.Technographics),
		txt(r.FundingStage),
		txt(r.CompanySizeBasis),
		txt(r.LinkedinUrl),
		txt(r.TwitterUrl),
		txt(r.FacebookUrl),
		txt(r.SecondaryEmails),
		txt(r.SecondaryPhones),
		txt(r.DecisionMakers),
		txt(r.GlassdoorRating),
		txt(r.HiringSignals),
		i4(r.EnrichmentAttempts),
		txt(r.EnrichmentWaterfall),
		txt(r.FieldProvenance),
		txt(r.Source),
		txt(r.SourceUrl),
		txt(r.CollectionJobID),
		r.WorkspaceID,
		i4(r.Score),
		txt(r.ScoreTier),
		txt(r.Status),
		txt(r.YupchaValueProp),
		txt(r.CompanyNeed),
		txt(r.Notes),
		txt(r.CreatedAt),
		txt(r.UpdatedAt),
		txt(r.LastEnrichedAt),
	}
	m := make(map[string]any, len(LeadKeys))
	for i, k := range LeadKeys {
		m[k] = vals[i]
	}
	return m
}

// maxPageSize caps nothing in Python; page arithmetic is clamped only to avoid
// int64 overflow on absurd inputs.
const maxInt64 = int64(^uint64(0) >> 1)

// QueryLeads ports PgLeadStore.query_leads_page: filtered, score DESC / id DESC
// page plus the unpaged total. page and pageSize below 1 are clamped to 1. Run
// inside tenancy.InWorkspace (leads is FORCE RLS).
func QueryLeads(ctx context.Context, tx pgx.Tx, f Filter, page, pageSize int) ([]map[string]any, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 1
	}
	q := gen.New(tx)
	count, page2 := f.params()
	total, err := q.CountLeadsFiltered(ctx, count)
	if err != nil {
		return nil, 0, err
	}
	limit := int64(pageSize)
	offset := maxInt64
	if int64(page-1) <= maxInt64/limit {
		offset = int64(page-1) * limit
	}
	page2.PageLimit, page2.PageOffset = limit, offset
	rows, err := q.QueryLeadsPage(ctx, page2)
	if err != nil {
		return nil, 0, err
	}
	out := make([]map[string]any, len(rows))
	for i, r := range rows {
		out[i] = leadRow(gen.LeadsByIDsRow(r))
	}
	return out, int(total), nil
}

// LeadsByIDs returns the leads with the given ids visible to the workspace, in
// ascending id order. Unknown or foreign ids are simply absent.
func LeadsByIDs(ctx context.Context, tx pgx.Tx, ids []int64) ([]map[string]any, error) {
	rows, err := gen.New(tx).LeadsByIDs(ctx, toInt32(ids))
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, len(rows))
	for i, r := range rows {
		out[i] = leadRow(r)
	}
	return out, nil
}
