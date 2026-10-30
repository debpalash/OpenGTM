package records

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/runplan"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
	"github.com/jackc/pgx/v5"
)

type Discovery struct {
	IDs           []string `json:"ids"`
	Titles        []string `json:"titles"`
	MaxPerCompany int      `json:"max_per_company"`
	MaxSearches   int      `json:"max_searches"`
}

// Discover builds a person-source workbook from an exact selection of stored
// companies. The existing durable source worker resolves every discovered person
// through the shared identity/employment operation.
func (s Store) Discover(ctx context.Context, ws string, b Discovery) (string, error) {
	if len(b.IDs) == 0 || len(b.IDs) > 100 || len(b.Titles) > 20 {
		return "", errors.New("select 1 to 100 companies and up to 20 titles")
	}
	if b.MaxPerCompany == 0 {
		b.MaxPerCompany = 5
	}
	if b.MaxSearches == 0 {
		b.MaxSearches = 20
	}
	id := wbcrud.NewID()
	e := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT id,coalesce(nullif(accepted_fields->>'domain',''),nullif(primary_domain,''),canonical_name) FROM company_entities WHERE workspace_id=$1 AND id=ANY($2::text[]) ORDER BY id`, ws, b.IDs)
		if e != nil {
			return e
		}
		companies := []string{}
		for rows.Next() {
			var rid, name string
			if e = rows.Scan(&rid, &name); e != nil {
				rows.Close()
				return e
			}
			companies = append(companies, name)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		unique := map[string]bool{}
		for _, v := range b.IDs {
			unique[v] = true
		}
		if len(companies) != len(unique) {
			return ErrNotFound
		}
		cfg := map[string]any{"id": "people_source", "name": "Find people", "type": "source", "kind": "people_search", "companies": companies, "titles": b.Titles, "max_per_company": b.MaxPerCompany, "max_searches": b.MaxSearches, "enabled": true}
		if _, e = runplan.ValidatePeopleSearchConfig(cfg); e != nil {
			return e
		}
		cols := []map[string]any{cfg}
		for _, field := range []string{"contact_person", "company", "website", "contact_title", "email", "linkedin_url"} {
			cols = append(cols, map[string]any{"id": field, "name": strings.ReplaceAll(field, "_", " "), "type": "lead_field", "lead_field": field})
		}
		raw, _ := json.Marshal(cols)
		_, e = tx.Exec(ctx, `INSERT INTO workbooks(id,workspace_id,name,description,status,source_type,source_config,columns_config,total_rows,completed_rows,sync_to_leads,budget_spent_usd,record_type) VALUES($1,$2,'Find people at selected companies','People linked to accepted company records','draft','records','{"row_storage_version":2}',$3,0,0,false,0,'person')`, id, ws, raw)
		return e
	})
	return id, e
}
