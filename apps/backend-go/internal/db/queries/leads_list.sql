-- Lead list query (port of PgLeadStore.get_leads, store.py). `leads` is FORCE
-- RLS: run inside tenancy.InWorkspace. The caller resolves order_by against the
-- Python allow-list before binding; the CASE chain below never interpolates it.
-- Unselected CASE arms are NULL for every row and therefore do not order.

-- name: ListLeads :many
SELECT id, company, website, email, email_confidence, email_provider, phone, phone_provider, contact_person, contact_title, city, state, address, specialization, company_size, employee_count_exact, description, revenue_range, founded_year, industry_tags, technologies, technographics, funding_stage, company_size_basis, linkedin_url, twitter_url, facebook_url, secondary_emails, secondary_phones, decision_makers, glassdoor_rating, hiring_signals, enrichment_attempts, enrichment_waterfall, field_provenance, source, source_url, collection_job_id, workspace_id, score, score_tier, status, yupcha_value_prop, company_need, notes, created_at, updated_at, last_enriched_at
FROM record_leads
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND (sqlc.narg('has_email')::bool IS NULL
       OR sqlc.narg('has_email')::bool = (email IS NOT NULL AND email <> ''))
  AND (sqlc.narg('has_phone')::bool IS NULL
       OR sqlc.narg('has_phone')::bool = (phone IS NOT NULL AND phone <> ''))
  AND (NOT @exclude_dead::bool OR status IS NULL OR status <> 'dead')
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
  AND (sqlc.narg('city')::text IS NULL OR city = sqlc.narg('city')::text)
  AND (sqlc.narg('source')::text IS NULL OR source = sqlc.narg('source')::text)
  AND (sqlc.narg('collection_job_id')::text IS NULL
       OR collection_job_id = sqlc.narg('collection_job_id')::text
       OR source = 'job:' || sqlc.narg('collection_job_id')::text)
  AND (sqlc.narg('score_min')::int4 IS NULL OR score >= sqlc.narg('score_min')::int4)
  AND (sqlc.narg('score_max')::int4 IS NULL OR score <= sqlc.narg('score_max')::int4)
  AND (sqlc.narg('score_tier')::text IS NULL OR score_tier = sqlc.narg('score_tier')::text)
  AND (sqlc.narg('company_size')::text IS NULL OR company_size = sqlc.narg('company_size')::text)
  AND (sqlc.narg('search')::text IS NULL
       OR search_tsv @@ websearch_to_tsquery('english', sqlc.narg('search')::text)
       OR company ILIKE '%' || sqlc.narg('search')::text || '%'
       OR city ILIKE '%' || sqlc.narg('search')::text || '%'
       OR specialization ILIKE '%' || sqlc.narg('search')::text || '%'
       OR notes ILIKE '%' || sqlc.narg('search')::text || '%'
       OR description ILIKE '%' || sqlc.narg('search')::text || '%')
ORDER BY
         CASE WHEN sqlc.arg('order_by')::text = 'score DESC' THEN score END DESC,
         CASE WHEN sqlc.arg('order_by')::text = 'score ASC' THEN score END ASC,
         CASE WHEN sqlc.arg('order_by')::text = 'company DESC' THEN company END DESC,
         CASE WHEN sqlc.arg('order_by')::text = 'company ASC' THEN company END ASC,
         CASE WHEN sqlc.arg('order_by')::text = 'created_at DESC' THEN created_at END DESC,
         CASE WHEN sqlc.arg('order_by')::text = 'created_at ASC' THEN created_at END ASC,
         CASE WHEN sqlc.arg('order_by')::text = 'updated_at DESC' THEN updated_at END DESC,
         CASE WHEN sqlc.arg('order_by')::text = 'updated_at ASC' THEN updated_at END ASC,
         CASE WHEN sqlc.arg('order_by')::text = 'city DESC' THEN city END DESC,
         CASE WHEN sqlc.arg('order_by')::text = 'city ASC' THEN city END ASC,
         CASE WHEN sqlc.arg('order_by')::text = 'status DESC' THEN status END DESC,
         CASE WHEN sqlc.arg('order_by')::text = 'status ASC' THEN status END ASC,
         CASE WHEN sqlc.arg('order_by')::text LIKE '% DESC' THEN id END DESC,
         CASE WHEN sqlc.arg('order_by')::text LIKE '% ASC' THEN id END ASC
LIMIT @page_limit::bigint OFFSET @page_offset::bigint;
