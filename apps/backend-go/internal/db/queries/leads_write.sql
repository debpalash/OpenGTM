-- Lead write store (port of PgLeadStore write paths in store.py). `leads` is
-- FORCE RLS: run inside tenancy.InWorkspace. Dynamic field updates live in
-- internal/leadswrite/fields.go (pgx, allow-listed columns).

-- name: InsertLeadCreateOnly :one
-- Create-only insert: every column not supplied takes the Python Lead
-- dataclass default. A collision on (workspace_id, company, city) returns no
-- row (pgx.ErrNoRows) and modifies nothing.
INSERT INTO leads (
  workspace_id,
  company,
  website,
  email,
  email_confidence,
  email_provider,
  phone,
  phone_provider,
  contact_person,
  contact_title,
  city,
  state,
  address,
  specialization,
  company_size,
  employee_count_exact,
  description,
  revenue_range,
  founded_year,
  founding_year,
  industry_tags,
  technologies,
  funding_stage,
  linkedin_url,
  twitter_url,
  facebook_url,
  secondary_emails,
  secondary_phones,
  decision_makers,
  glassdoor_rating,
  hiring_signals,
  enrichment_attempts,
  enrichment_waterfall,
  last_funding_amount,
  investors,
  recent_news,
  google_rating,
  revenue_estimate,
  email_verify,
  email_presence,
  source,
  score,
  score_tier,
  status,
  yupcha_value_prop,
  company_need,
  notes,
  created_at,
  updated_at,
  last_enriched_at,
  company_size_basis,
  field_provenance,
  technographics,
  source_url,
  collection_job_id
) VALUES (
  sqlc.arg(workspace_id)::text,
  sqlc.arg(company)::text,
  sqlc.arg(website)::text,
  sqlc.arg(email)::text,
  '',
  '',
  sqlc.arg(phone)::text,
  '',
  sqlc.arg(contact_person)::text,
  '',
  sqlc.arg(city)::text,
  '',
  '',
  sqlc.arg(specialization)::text,
  '',
  0,
  '',
  '',
  '',
  '',
  '',
  '',
  '',
  sqlc.arg(linkedin_url)::text,
  '',
  '',
  '',
  '',
  '',
  '',
  '',
  0,
  '',
  '',
  '',
  '',
  '',
  '',
  '',
  '',
  sqlc.arg(source)::text,
  0,
  'unqualified',
  'new',
  '',
  '',
  sqlc.arg(notes)::text,
  sqlc.arg(created_at)::text,
  sqlc.arg(updated_at)::text,
  '',
  '',
  '',
  '',
  '',
  ''
)
ON CONFLICT (workspace_id, company, city) DO NOTHING
RETURNING id;

-- name: LeadIDByKey :one
SELECT id FROM leads
WHERE workspace_id = sqlc.arg(workspace_id)::text AND company = sqlc.arg(company)::text AND city = sqlc.arg(city)::text;

-- name: UpdateLeadStatus :execrows
UPDATE leads SET status = sqlc.arg(status)::text, updated_at = sqlc.arg(updated_at)::text
WHERE workspace_id = current_setting('app.workspace_id', true) AND id = sqlc.arg(id)::int4;

-- name: DeleteLeadByID :execrows
DELETE FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true) AND id = sqlc.arg(id)::int4;

-- name: LeadExists :one
SELECT EXISTS (SELECT 1 FROM leads
  WHERE workspace_id = current_setting('app.workspace_id', true) AND id = sqlc.arg(id)::int4);
