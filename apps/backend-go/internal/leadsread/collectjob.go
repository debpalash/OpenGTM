package leadsread

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// CollectionJobLeads returns the caller's leads attached to a collection job, either through
// collection_job_id or through source = 'job:<id>' (score DESC, id DESC), at most limit rows, in the
// Lead.to_dict shape. The store query selects the ledger's column order; this maps it onto Lead so the
// JSON keys come out in Python's dataclass order. The result is never nil. Run inside tenancy.InWorkspace.
func CollectionJobLeads(ctx context.Context, tx pgx.Tx, jobID string, limit int) ([]Lead, error) {
	rows, err := gen.New(tx).LeadsByCollectionJob(ctx, gen.LeadsByCollectionJobParams{JobID: jobID, PageLimit: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]Lead, 0, len(rows))
	for _, r := range rows {
		out = append(out, Lead{
			ID: int64(r.ID), Company: r.Company, Website: txt(r.Website), Email: txt(r.Email),
			EmailConfidence: txt(r.EmailConfidence), EmailProvider: txt(r.EmailProvider), Phone: txt(r.Phone),
			PhoneProvider: txt(r.PhoneProvider), ContactPerson: txt(r.ContactPerson), ContactTitle: txt(r.ContactTitle),
			City: txt(r.City), State: txt(r.State), Address: txt(r.Address), Specialization: txt(r.Specialization),
			CompanySize: txt(r.CompanySize), EmployeeCountExact: i4(r.EmployeeCountExact), Description: txt(r.Description),
			RevenueRange: txt(r.RevenueRange), FoundedYear: txt(r.FoundedYear), IndustryTags: txt(r.IndustryTags),
			Technologies: txt(r.Technologies), Technographics: txt(r.Technographics), FundingStage: txt(r.FundingStage),
			CompanySizeBasis: txt(r.CompanySizeBasis), LinkedinURL: txt(r.LinkedinUrl), TwitterURL: txt(r.TwitterUrl),
			FacebookURL: txt(r.FacebookUrl), SecondaryEmails: txt(r.SecondaryEmails), SecondaryPhones: txt(r.SecondaryPhones),
			DecisionMakers: txt(r.DecisionMakers), GlassdoorRating: txt(r.GlassdoorRating), HiringSignals: txt(r.HiringSignals),
			EnrichmentAttempts: i4(r.EnrichmentAttempts), EnrichmentWaterfall: txt(r.EnrichmentWaterfall),
			FieldProvenance: txt(r.FieldProvenance), Source: txt(r.Source), SourceURL: txt(r.SourceUrl),
			CollectionJobID: txt(r.CollectionJobID), WorkspaceID: r.WorkspaceID, Score: i4(r.Score), ScoreTier: txt(r.ScoreTier),
			Status: txt(r.Status), YupchaValueProp: txt(r.YupchaValueProp), CompanyNeed: txt(r.CompanyNeed), Notes: txt(r.Notes),
			CreatedAt: txt(r.CreatedAt), UpdatedAt: txt(r.UpdatedAt), LastEnrichedAt: txt(r.LastEnrichedAt),
		})
	}
	return out, nil
}
