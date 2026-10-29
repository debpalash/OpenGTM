package leadsread

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// ListParams mirrors PgLeadStore.get_leads arguments (store.py). String
// filters treat "" as unset (Python truthiness); pointer filters treat nil as
// unset. Limit is used as given (callers pass Python's default of 500).
type ListParams struct {
	Status, City, Source, ScoreTier, Search, CompanySize, CollectionJobID string
	ScoreMin, ScoreMax                                                    *int
	HasEmail, HasPhone                                                    *bool
	Limit, Offset                                                         int
	OrderBy                                                               string
	ExcludeDead                                                           bool
}

// allowedOrders is store.py _ALLOWED_ORDERS.
var allowedOrders = map[string]bool{
	"score DESC": true, "score ASC": true, "company ASC": true, "company DESC": true,
	"created_at DESC": true, "created_at ASC": true, "updated_at DESC": true,
	"city ASC": true, "city DESC": true, "status ASC": true,
}

// ListLeads ports PgLeadStore.get_leads. An OrderBy outside the allow-list
// falls back to "score DESC"; the id tiebreaker follows the same direction.
// Run inside tenancy.InWorkspace.
func ListLeads(ctx context.Context, tx pgx.Tx, p ListParams) ([]Lead, error) {
	order := p.OrderBy
	if !allowedOrders[order] {
		order = "score DESC"
	}
	limit, offset := int64(p.Limit), int64(p.Offset)
	if limit < 0 {
		limit = 0
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := gen.New(tx).ListLeads(ctx, gen.ListLeadsParams{
		HasEmail:        pbool(p.HasEmail),
		HasPhone:        pbool(p.HasPhone),
		ExcludeDead:     p.ExcludeDead,
		Status:          ptxt(p.Status),
		City:            ptxt(p.City),
		Source:          ptxt(p.Source),
		CollectionJobID: ptxt(p.CollectionJobID),
		ScoreMin:        pint(p.ScoreMin),
		ScoreMax:        pint(p.ScoreMax),
		ScoreTier:       ptxt(p.ScoreTier),
		CompanySize:     ptxt(p.CompanySize),
		Search:          ptxt(p.Search),
		OrderBy:         order,
		PageOffset:      offset,
		PageLimit:       limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Lead, len(rows))
	for i, r := range rows {
		out[i] = leadRow(gen.LeadsByIDsRow(r))
	}
	return out, nil
}

// SimilarLeads ports the similar_leads route query: search by the reference's
// specialization (limit+1), and only when that finds nothing fall back to its
// city; then drop the reference and cut to limit.
func SimilarLeads(ctx context.Context, tx pgx.Tx, ref Lead, limit int) ([]Lead, error) {
	var similar []Lead
	var err error
	if ref.Specialization != nil && *ref.Specialization != "" {
		similar, err = ListLeads(ctx, tx, ListParams{Search: *ref.Specialization, Limit: limit + 1})
		if err != nil {
			return nil, err
		}
	}
	if len(similar) == 0 && ref.City != nil && *ref.City != "" {
		similar, err = ListLeads(ctx, tx, ListParams{City: *ref.City, Limit: limit + 1})
		if err != nil {
			return nil, err
		}
	}
	out := make([]Lead, 0, len(similar))
	for _, l := range similar {
		if l.ID != ref.ID {
			out = append(out, l)
		}
	}
	if limit < 0 { // Python slice [:limit] with a negative limit drops from the end
		if n := len(out) + limit; n > 0 {
			return out[:n], nil
		}
		return out[:0], nil
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
