-- plan 14: leads of one collection job (port of PgJobLedger.get_job_leads).
-- leads is FORCE RLS: run inside tenancy.InWorkspace. The Python store orders by
-- score DESC only; the id tiebreak is documented hardening for stable output.

-- name: LeadsByCollectionJob :many
SELECT id, company, website, email, email_confidence, email_provider, phone, phone_provider, contact_person, contact_title, city, state, address, specialization, company_size, employee_count_exact, description, revenue_range, founded_year, industry_tags, technologies, technographics, funding_stage, company_size_basis, linkedin_url, twitter_url, facebook_url, secondary_emails, secondary_phones, decision_makers, glassdoor_rating, hiring_signals, enrichment_attempts, enrichment_waterfall, field_provenance, source, source_url, collection_job_id, workspace_id, score, score_tier, status, yupcha_value_prop, company_need, notes, created_at, updated_at, last_enriched_at
FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND (collection_job_id = sqlc.arg(job_id)::text OR source = 'job:' || sqlc.arg(job_id)::text)
ORDER BY score DESC, id DESC
LIMIT sqlc.arg(page_limit)::bigint;
