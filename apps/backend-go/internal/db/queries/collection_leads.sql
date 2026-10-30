-- plan 14: leads of one collection job (port of PgJobLedger.get_job_leads).
-- leads is FORCE RLS: run inside tenancy.InWorkspace. The Python store orders by
-- score DESC only; the id tiebreak is documented hardening for stable output.

-- name: LeadsByCollectionJob :many
SELECT id, company, website, email, phone, contact_person, contact_title, city, state, specialization, company_size, description, linkedin_url, twitter_url, source, source_url, collection_job_id, score, score_tier, status, yupcha_value_prop, company_need, notes, created_at, updated_at, last_enriched_at, workspace_id, address, employee_count_exact, revenue_range, founded_year, industry_tags, technologies, technographics, funding_stage, company_size_basis, secondary_emails, secondary_phones, decision_makers, glassdoor_rating, email_confidence, email_provider, phone_provider, facebook_url, hiring_signals, enrichment_attempts, enrichment_waterfall, field_provenance, founding_year, last_funding_amount, investors, recent_news, google_rating, revenue_estimate, email_verify, email_presence
FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND (collection_job_id = sqlc.arg(job_id)::text OR source = 'job:' || sqlc.arg(job_id)::text)
ORDER BY score DESC, id DESC
LIMIT sqlc.arg(page_limit)::bigint;

-- name: CJDeleteLeads :execrows
-- Delete handler (keep_leads = false): remove the job's leads before CJDelete,
-- like PgJobLedger.delete_job. Workspace-filtered; run inside tenancy.InWorkspace.
DELETE FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND (collection_job_id = sqlc.arg(job_id)::text OR source = 'job:' || sqlc.arg(job_id)::text);
