package leadsread

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Lead is one lead row in the shape of Python's Lead.to_dict: json.Marshal
// emits the keys in the Lead dataclass order ("id" first). Nullable columns are
// pointers so DB NULL serialises as null (Python passes NULL through
// Lead.from_dict unchanged). search_tsv and founding_year are not Lead fields.
type Lead struct {
	ID                  int64   `json:"id"`
	Company             string  `json:"company"`
	Website             *string `json:"website"`
	Email               *string `json:"email"`
	EmailConfidence     *string `json:"email_confidence"`
	EmailProvider       *string `json:"email_provider"`
	Phone               *string `json:"phone"`
	PhoneProvider       *string `json:"phone_provider"`
	ContactPerson       *string `json:"contact_person"`
	ContactTitle        *string `json:"contact_title"`
	City                *string `json:"city"`
	State               *string `json:"state"`
	Address             *string `json:"address"`
	Specialization      *string `json:"specialization"`
	CompanySize         *string `json:"company_size"`
	EmployeeCountExact  *int64  `json:"employee_count_exact"`
	Description         *string `json:"description"`
	RevenueRange        *string `json:"revenue_range"`
	FoundedYear         *string `json:"founded_year"`
	IndustryTags        *string `json:"industry_tags"`
	Technologies        *string `json:"technologies"`
	Technographics      *string `json:"technographics"`
	FundingStage        *string `json:"funding_stage"`
	CompanySizeBasis    *string `json:"company_size_basis"`
	LinkedinURL         *string `json:"linkedin_url"`
	TwitterURL          *string `json:"twitter_url"`
	FacebookURL         *string `json:"facebook_url"`
	SecondaryEmails     *string `json:"secondary_emails"`
	SecondaryPhones     *string `json:"secondary_phones"`
	DecisionMakers      *string `json:"decision_makers"`
	GlassdoorRating     *string `json:"glassdoor_rating"`
	HiringSignals       *string `json:"hiring_signals"`
	EnrichmentAttempts  *int64  `json:"enrichment_attempts"`
	EnrichmentWaterfall *string `json:"enrichment_waterfall"`
	FieldProvenance     *string `json:"field_provenance"`
	Source              *string `json:"source"`
	SourceURL           *string `json:"source_url"`
	CollectionJobID     *string `json:"collection_job_id"`
	WorkspaceID         string  `json:"workspace_id"`
	Score               *int64  `json:"score"`
	ScoreTier           *string `json:"score_tier"`
	Status              *string `json:"status"`
	YupchaValueProp     *string `json:"yupcha_value_prop"`
	CompanyNeed         *string `json:"company_need"`
	Notes               *string `json:"notes"`
	CreatedAt           *string `json:"created_at"`
	UpdatedAt           *string `json:"updated_at"`
	LastEnrichedAt      *string `json:"last_enriched_at"`
}

// leadIndex maps a lead field name to its position in Lead.Values.
var leadIndex = map[string]int{
	"id":                   0,
	"company":              1,
	"website":              2,
	"email":                3,
	"email_confidence":     4,
	"email_provider":       5,
	"phone":                6,
	"phone_provider":       7,
	"contact_person":       8,
	"contact_title":        9,
	"city":                 10,
	"state":                11,
	"address":              12,
	"specialization":       13,
	"company_size":         14,
	"employee_count_exact": 15,
	"description":          16,
	"revenue_range":        17,
	"founded_year":         18,
	"industry_tags":        19,
	"technologies":         20,
	"technographics":       21,
	"funding_stage":        22,
	"company_size_basis":   23,
	"linkedin_url":         24,
	"twitter_url":          25,
	"facebook_url":         26,
	"secondary_emails":     27,
	"secondary_phones":     28,
	"decision_makers":      29,
	"glassdoor_rating":     30,
	"hiring_signals":       31,
	"enrichment_attempts":  32,
	"enrichment_waterfall": 33,
	"field_provenance":     34,
	"source":               35,
	"source_url":           36,
	"collection_job_id":    37,
	"workspace_id":         38,
	"score":                39,
	"score_tier":           40,
	"status":               41,
	"yupcha_value_prop":    42,
	"company_need":         43,
	"notes":                44,
	"created_at":           45,
	"updated_at":           46,
	"last_enriched_at":     47,
}

// Values returns the field values in Lead.to_dict key order (nil for NULL).
func (l Lead) Values() []any {
	vals := []any{nz(l.ID), nz(l.Company), nz(l.Website), nz(l.Email), nz(l.EmailConfidence), nz(l.EmailProvider), nz(l.Phone), nz(l.PhoneProvider), nz(l.ContactPerson), nz(l.ContactTitle), nz(l.City), nz(l.State), nz(l.Address), nz(l.Specialization), nz(l.CompanySize), nz(l.EmployeeCountExact), nz(l.Description), nz(l.RevenueRange), nz(l.FoundedYear), nz(l.IndustryTags), nz(l.Technologies), nz(l.Technographics), nz(l.FundingStage), nz(l.CompanySizeBasis), nz(l.LinkedinURL), nz(l.TwitterURL), nz(l.FacebookURL), nz(l.SecondaryEmails), nz(l.SecondaryPhones), nz(l.DecisionMakers), nz(l.GlassdoorRating), nz(l.HiringSignals), nz(l.EnrichmentAttempts), nz(l.EnrichmentWaterfall), nz(l.FieldProvenance), nz(l.Source), nz(l.SourceURL), nz(l.CollectionJobID), nz(l.WorkspaceID), nz(l.Score), nz(l.ScoreTier), nz(l.Status), nz(l.YupchaValueProp), nz(l.CompanyNeed), nz(l.Notes), nz(l.CreatedAt), nz(l.UpdatedAt), nz(l.LastEnrichedAt)}
	return vals
}

// Get returns a field by its Python/column name; ok is false for unknown names.
// NULL columns return (nil, true).
func (l Lead) Get(key string) (any, bool) {
	i, ok := leadIndex[key]
	if !ok {
		return nil, false
	}
	return l.Values()[i], true
}

// nz turns a nil *string / *int64 into an untyped nil.
func nz(v any) any {
	switch p := v.(type) {
	case *string:
		if p == nil {
			return nil
		}
		return *p
	case *int64:
		if p == nil {
			return nil
		}
		return *p
	}
	return v
}

func txt(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func i4(n pgtype.Int4) *int64 {
	if !n.Valid {
		return nil
	}
	v := int64(n.Int32)
	return &v
}

func leadRow(r gen.LeadsByIDsRow) Lead {
	return Lead{
		ID:                  int64(r.ID),
		Company:             r.Company,
		Website:             txt(r.Website),
		Email:               txt(r.Email),
		EmailConfidence:     txt(r.EmailConfidence),
		EmailProvider:       txt(r.EmailProvider),
		Phone:               txt(r.Phone),
		PhoneProvider:       txt(r.PhoneProvider),
		ContactPerson:       txt(r.ContactPerson),
		ContactTitle:        txt(r.ContactTitle),
		City:                txt(r.City),
		State:               txt(r.State),
		Address:             txt(r.Address),
		Specialization:      txt(r.Specialization),
		CompanySize:         txt(r.CompanySize),
		EmployeeCountExact:  i4(r.EmployeeCountExact),
		Description:         txt(r.Description),
		RevenueRange:        txt(r.RevenueRange),
		FoundedYear:         txt(r.FoundedYear),
		IndustryTags:        txt(r.IndustryTags),
		Technologies:        txt(r.Technologies),
		Technographics:      txt(r.Technographics),
		FundingStage:        txt(r.FundingStage),
		CompanySizeBasis:    txt(r.CompanySizeBasis),
		LinkedinURL:         txt(r.LinkedinUrl),
		TwitterURL:          txt(r.TwitterUrl),
		FacebookURL:         txt(r.FacebookUrl),
		SecondaryEmails:     txt(r.SecondaryEmails),
		SecondaryPhones:     txt(r.SecondaryPhones),
		DecisionMakers:      txt(r.DecisionMakers),
		GlassdoorRating:     txt(r.GlassdoorRating),
		HiringSignals:       txt(r.HiringSignals),
		EnrichmentAttempts:  i4(r.EnrichmentAttempts),
		EnrichmentWaterfall: txt(r.EnrichmentWaterfall),
		FieldProvenance:     txt(r.FieldProvenance),
		Source:              txt(r.Source),
		SourceURL:           txt(r.SourceUrl),
		CollectionJobID:     txt(r.CollectionJobID),
		WorkspaceID:         r.WorkspaceID,
		Score:               i4(r.Score),
		ScoreTier:           txt(r.ScoreTier),
		Status:              txt(r.Status),
		YupchaValueProp:     txt(r.YupchaValueProp),
		CompanyNeed:         txt(r.CompanyNeed),
		Notes:               txt(r.Notes),
		CreatedAt:           txt(r.CreatedAt),
		UpdatedAt:           txt(r.UpdatedAt),
		LastEnrichedAt:      txt(r.LastEnrichedAt),
	}
}

// maxInt64 bounds page offset arithmetic (Python has no page cap; this only
// avoids int64 overflow on absurd inputs).
const maxInt64 = int64(^uint64(0) >> 1)

// QueryLeads ports PgLeadStore.query_leads_page: filtered, score DESC / id DESC
// page plus the unpaged total. page and pageSize below 1 are clamped to 1. Run
// inside tenancy.InWorkspace (leads is FORCE RLS).
func QueryLeads(ctx context.Context, tx pgx.Tx, f Filter, page, pageSize int) ([]Lead, int, error) {
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
	out := make([]Lead, len(rows))
	for i, r := range rows {
		out[i] = leadRow(gen.LeadsByIDsRow(r))
	}
	return out, int(total), nil
}

// LeadsByIDs returns the leads with the given ids visible to the workspace, in
// ascending id order. Unknown or foreign ids are simply absent.
func LeadsByIDs(ctx context.Context, tx pgx.Tx, ids []int64) ([]Lead, error) {
	rows, err := gen.New(tx).LeadsByIDs(ctx, toInt32(ids))
	if err != nil {
		return nil, err
	}
	out := make([]Lead, len(rows))
	for i, r := range rows {
		out[i] = leadRow(r)
	}
	return out, nil
}
