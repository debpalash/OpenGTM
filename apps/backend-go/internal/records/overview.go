package records

import (
	"context"
	"encoding/json"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/jackc/pgx/v5"
)

// Overview reads canonical record facts and execution state in one MVCC snapshot.
// Verified coverage is people whose accepted email verification status is explicit.
func (s Store) Overview(ctx context.Context, ws string) (json.RawMessage, error) {
	var out json.RawMessage
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT jsonb_build_object(
   'companies',(SELECT count(*) FROM company_entities WHERE workspace_id=$1),
   'people',(SELECT count(*) FROM person_entities WHERE workspace_id=$1),
   'people_with_email',(SELECT count(*) FROM person_entities WHERE workspace_id=$1 AND lower(trim(coalesce(accepted_fields->>'email',''))) NOT IN ('','n/a')),
   'verified_emails',(SELECT count(*) FROM person_entities WHERE workspace_id=$1 AND lower(trim(coalesce(accepted_fields->>'email',''))) NOT IN ('','n/a') AND lower(coalesce(nullif(accepted_fields->>'email_verify_status',''),accepted_fields->'email_verify'->>'status',accepted_fields->>'email_verify','')) IN ('valid','verified','deliverable')),
   'companies_needing_domain',(SELECT count(*) FROM company_entities WHERE workspace_id=$1 AND nullif(trim(coalesce(accepted_fields->>'domain',primary_domain,'')),'') IS NULL),
   'running_workbooks',(SELECT count(*) FROM workbooks WHERE workspace_id=$1 AND status='running'),
   'failed_workbooks',(SELECT count(*) FROM workbooks WHERE workspace_id=$1 AND status='failed'),
   'workbooks',coalesce((SELECT jsonb_agg(to_jsonb(w)) FROM (SELECT id,name,coalesce(status,'draft') AS status,coalesce(total_rows,0) AS total_rows,coalesce(completed_rows,0) AS completed_rows,coalesce(updated_at,created_at,now()) AS updated_at FROM workbooks WHERE workspace_id=$1 ORDER BY updated_at DESC NULLS LAST,id LIMIT 6) w),'[]'),
   'imports',coalesce((SELECT jsonb_agg(to_jsonb(i)) FROM (SELECT source,coalesce((result->>'imported')::int,0) AS imported,coalesce((result->>'rejected')::int,0) AS rejected,created_at FROM record_imports WHERE workspace_id=$1 AND result?'imported' ORDER BY created_at DESC,id LIMIT 4) i),'[]'),
   'sampled_at',now())`, ws).Scan(&out)
	})
	return out, err
}
