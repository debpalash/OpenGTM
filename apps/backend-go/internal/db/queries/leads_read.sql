-- Read-only lead queries (port of PgLeadStore reads in store.py). `leads` is
-- FORCE RLS: run inside tenancy.InWorkspace. workspace_id is also filtered
-- explicitly (belt) from the transaction-local GUC, like the Python store.

-- name: CountLeadsFiltered :one
SELECT count(*)::bigint
FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND (NOT @has_lead_ids::bool OR id = ANY(@lead_ids::int4[]))
  AND (sqlc.narg('city')::text IS NULL OR city = sqlc.narg('city')::text)
  AND (sqlc.narg('state')::text IS NULL OR state = sqlc.narg('state')::text)
  AND (sqlc.narg('score_tier')::text IS NULL OR score_tier = sqlc.narg('score_tier')::text)
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
  AND (sqlc.narg('source')::text IS NULL OR source = sqlc.narg('source')::text)
  AND (sqlc.narg('company_size')::text IS NULL OR company_size = sqlc.narg('company_size')::text)
  AND (NOT @has_job_ids::bool
       OR collection_job_id = ANY(@job_ids::text[])
       OR source = ANY(@job_sources::text[]))
  AND (sqlc.narg('specialization')::text IS NULL
       OR specialization ILIKE '%' || sqlc.narg('specialization')::text || '%')
  AND (sqlc.narg('has_email')::bool IS NULL
       OR sqlc.narg('has_email')::bool = (email IS NOT NULL AND email <> ''))
  AND (sqlc.narg('has_phone')::bool IS NULL
       OR sqlc.narg('has_phone')::bool = (phone IS NOT NULL AND phone <> ''))
  AND (sqlc.narg('has_website')::bool IS NULL
       OR sqlc.narg('has_website')::bool = (website IS NOT NULL AND website <> ''))
  AND (sqlc.narg('min_score')::int4 IS NULL OR score >= sqlc.narg('min_score')::int4)
  AND (sqlc.narg('max_score')::int4 IS NULL OR score <= sqlc.narg('max_score')::int4)
  AND (sqlc.narg('search')::text IS NULL
       OR search_tsv @@ websearch_to_tsquery('english', sqlc.narg('search')::text)
       OR company ILIKE '%' || sqlc.narg('search')::text || '%'
       OR city ILIKE '%' || sqlc.narg('search')::text || '%'
       OR specialization ILIKE '%' || sqlc.narg('search')::text || '%'
       OR notes ILIKE '%' || sqlc.narg('search')::text || '%'
       OR description ILIKE '%' || sqlc.narg('search')::text || '%');

-- name: QueryLeadsPage :many
SELECT id, company, website, email, email_confidence, email_provider, phone, phone_provider, contact_person, contact_title, city, state, address, specialization, company_size, employee_count_exact, description, revenue_range, founded_year, industry_tags, technologies, technographics, funding_stage, company_size_basis, linkedin_url, twitter_url, facebook_url, secondary_emails, secondary_phones, decision_makers, glassdoor_rating, hiring_signals, enrichment_attempts, enrichment_waterfall, field_provenance, source, source_url, collection_job_id, workspace_id, score, score_tier, status, yupcha_value_prop, company_need, notes, created_at, updated_at, last_enriched_at
FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND (NOT @has_lead_ids::bool OR id = ANY(@lead_ids::int4[]))
  AND (sqlc.narg('city')::text IS NULL OR city = sqlc.narg('city')::text)
  AND (sqlc.narg('state')::text IS NULL OR state = sqlc.narg('state')::text)
  AND (sqlc.narg('score_tier')::text IS NULL OR score_tier = sqlc.narg('score_tier')::text)
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
  AND (sqlc.narg('source')::text IS NULL OR source = sqlc.narg('source')::text)
  AND (sqlc.narg('company_size')::text IS NULL OR company_size = sqlc.narg('company_size')::text)
  AND (NOT @has_job_ids::bool
       OR collection_job_id = ANY(@job_ids::text[])
       OR source = ANY(@job_sources::text[]))
  AND (sqlc.narg('specialization')::text IS NULL
       OR specialization ILIKE '%' || sqlc.narg('specialization')::text || '%')
  AND (sqlc.narg('has_email')::bool IS NULL
       OR sqlc.narg('has_email')::bool = (email IS NOT NULL AND email <> ''))
  AND (sqlc.narg('has_phone')::bool IS NULL
       OR sqlc.narg('has_phone')::bool = (phone IS NOT NULL AND phone <> ''))
  AND (sqlc.narg('has_website')::bool IS NULL
       OR sqlc.narg('has_website')::bool = (website IS NOT NULL AND website <> ''))
  AND (sqlc.narg('min_score')::int4 IS NULL OR score >= sqlc.narg('min_score')::int4)
  AND (sqlc.narg('max_score')::int4 IS NULL OR score <= sqlc.narg('max_score')::int4)
  AND (sqlc.narg('search')::text IS NULL
       OR search_tsv @@ websearch_to_tsquery('english', sqlc.narg('search')::text)
       OR company ILIKE '%' || sqlc.narg('search')::text || '%'
       OR city ILIKE '%' || sqlc.narg('search')::text || '%'
       OR specialization ILIKE '%' || sqlc.narg('search')::text || '%'
       OR notes ILIKE '%' || sqlc.narg('search')::text || '%'
       OR description ILIKE '%' || sqlc.narg('search')::text || '%')
ORDER BY score DESC, id DESC
LIMIT @page_limit::bigint OFFSET @page_offset::bigint;

-- name: LeadsByIDs :many
SELECT id, company, website, email, email_confidence, email_provider, phone, phone_provider, contact_person, contact_title, city, state, address, specialization, company_size, employee_count_exact, description, revenue_range, founded_year, industry_tags, technologies, technographics, funding_stage, company_size_basis, linkedin_url, twitter_url, facebook_url, secondary_emails, secondary_phones, decision_makers, glassdoor_rating, hiring_signals, enrichment_attempts, enrichment_waterfall, field_provenance, source, source_url, collection_job_id, workspace_id, score, score_tier, status, yupcha_value_prop, company_need, notes, created_at, updated_at, last_enriched_at
FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND id = ANY(@ids::int4[])
ORDER BY id;

-- name: LeadStatsTotals :one
SELECT count(*)::bigint AS total,
  count(*) FILTER (WHERE email IS NOT NULL AND email <> '' AND email <> 'N/A')::bigint AS with_email,
  count(*) FILTER (WHERE phone IS NOT NULL AND phone <> '' AND phone <> 'N/A')::bigint AS with_phone,
  count(*) FILTER (WHERE website IS NOT NULL AND website <> '' AND website <> 'N/A')::bigint AS with_website,
  count(*) FILTER (WHERE linkedin_url IS NOT NULL AND linkedin_url <> '' AND linkedin_url <> 'N/A')::bigint AS with_linkedin,
  count(*) FILTER (WHERE contact_person IS NOT NULL AND contact_person <> '' AND contact_person <> 'N/A')::bigint AS with_contact,
  coalesce(avg(score), 0)::float8 AS avg_score
FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true);

-- name: LeadCountsByStatus :many
SELECT status AS key, count(*)::bigint AS n FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true)
GROUP BY status ORDER BY status;

-- name: LeadCountsByTier :many
SELECT score_tier AS key, count(*)::bigint AS n FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true)
GROUP BY score_tier ORDER BY score_tier;

-- name: LeadCountsBySource :many
SELECT source AS key, count(*)::bigint AS n FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true)
GROUP BY source ORDER BY source;

-- name: LeadCountsByCity :many
SELECT city AS key, count(*)::bigint AS n FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true)
GROUP BY city ORDER BY count(*) DESC, city LIMIT 15;

-- name: LeadDistinctCities :many
SELECT DISTINCT city AS value FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND city IS NOT NULL AND city <> ''
ORDER BY city;

-- name: LeadDistinctTiers :many
SELECT DISTINCT score_tier AS value FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND score_tier IS NOT NULL AND score_tier <> ''
ORDER BY score_tier;

-- name: LeadDistinctSources :many
SELECT DISTINCT source AS value FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND source IS NOT NULL AND source <> ''
ORDER BY source;

-- name: LeadDistinctStatuses :many
SELECT DISTINCT status AS value FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND status IS NOT NULL AND status <> ''
ORDER BY status;

-- name: LeadDistinctSpecializations :many
SELECT DISTINCT specialization AS value FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND specialization IS NOT NULL AND specialization <> ''
ORDER BY specialization LIMIT 50;
