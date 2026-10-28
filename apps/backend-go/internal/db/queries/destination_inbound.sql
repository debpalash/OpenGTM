-- plan 10 task 10: destination inbound tokens and CRM reconcile.
-- destination_inbound_tokens is a non-RLS auth-plane table (looked up by hash on
-- the plain pool); receipts, deliveries and leads are RLS tables: run inside
-- tenancy.InWorkspace and keep the explicit workspace predicate.
-- Naive timestamp columns (created_at, revoked_at) hold UTC wall time: revoked_at is
-- converted with AT TIME ZONE 'UTC', never through the session TimeZone.

-- name: DestInboundTokenInsert :one
INSERT INTO destination_inbound_tokens (id, workspace_id, destination_id, token_hash, prefix, created_by, expires_at)
VALUES (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(destination_id)::text,
        sqlc.arg(token_hash)::text, sqlc.arg(prefix)::text, sqlc.narg(created_by)::int, sqlc.arg(expires_at)::timestamptz)
RETURNING created_at;

-- name: DestInboundTokensRevokeActive :execrows
UPDATE destination_inbound_tokens SET revoked_at = (sqlc.arg(now)::timestamptz AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND destination_id = sqlc.arg(destination_id)::text
  AND revoked_at IS NULL;

-- name: DestInboundTokenLatestActive :one
SELECT prefix, created_by, created_at, expires_at, last_used_at
FROM destination_inbound_tokens
WHERE workspace_id = sqlc.arg(workspace_id)::text AND destination_id = sqlc.arg(destination_id)::text
  AND revoked_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: DestInboundTokenByHash :one
SELECT id, workspace_id, destination_id, token_hash, expires_at, revoked_at
FROM destination_inbound_tokens
WHERE token_hash = sqlc.arg(token_hash)::text;

-- Atomic recheck of revocation, destination binding and expiry while stamping.
-- name: DestInboundTokenStamp :one
UPDATE destination_inbound_tokens SET last_used_at = sqlc.arg(now)::timestamptz
WHERE id = sqlc.arg(id)::text AND destination_id = sqlc.arg(destination_id)::text
  AND revoked_at IS NULL AND expires_at IS NOT NULL AND expires_at > sqlc.arg(now)::timestamptz
RETURNING workspace_id;

-- name: DestInboundReceiptByEvent :one
SELECT id, destination_id, provider, external_event_id, external_record_id, lead_id, company_entity_id,person_entity_id,status,
       conflict_policy, applied_fields, ignored_fields, error, created_at
FROM destination_inbound_receipts
WHERE workspace_id = sqlc.arg(workspace_id)::text AND destination_id = sqlc.arg(destination_id)::text
  AND external_event_id = sqlc.arg(external_event_id)::text;

-- name: DestInboundReceiptInsert :one
INSERT INTO destination_inbound_receipts (id, workspace_id, destination_id, provider, external_event_id,
  external_record_id, lead_id, status, conflict_policy, applied_fields, ignored_fields, payload_fingerprint, error)
VALUES (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(destination_id)::text, sqlc.arg(provider)::text,
  sqlc.arg(external_event_id)::text, sqlc.narg(external_record_id)::text, sqlc.narg(lead_id)::int,
  sqlc.arg(status)::text, sqlc.arg(conflict_policy)::text, sqlc.arg(applied_fields)::json,
  sqlc.arg(ignored_fields)::json, sqlc.arg(payload_fingerprint)::text, sqlc.narg(error)::text)
RETURNING id, destination_id, provider, external_event_id, external_record_id, lead_id, company_entity_id,person_entity_id,status,
          conflict_policy, applied_fields, ignored_fields, error, created_at;

-- Python (on PG) ordered delivered_at DESC with NULLs first; keep that, id breaks ties.
-- name: DestInboundDeliveryLead :one
SELECT lead_id FROM destination_deliveries
WHERE workspace_id = sqlc.arg(workspace_id)::text AND destination_id = sqlc.arg(destination_id)::text
  AND external_id = sqlc.arg(external_id)::text AND status = 'success'
ORDER BY delivered_at DESC, id DESC
LIMIT 1;

-- name: DestInboundLeadsByEmail :many
SELECT id FROM record_leads
WHERE workspace_id = sqlc.arg(workspace_id)::text AND email = sqlc.arg(email)::text
ORDER BY id
LIMIT 2;

-- name: DestInboundLeadLock :one
SELECT company, website, email, phone, contact_person, contact_title, city, state, address,
       specialization, company_size, employee_count_exact, description, revenue_range, founded_year,
       industry_tags, technologies, funding_stage, linkedin_url, twitter_url, facebook_url,
       secondary_emails, secondary_phones, decision_makers, status, notes
FROM record_leads
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint
FOR UPDATE;

-- name: DestInboundSetLead_company :exec
UPDATE leads SET company = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_website :exec
UPDATE leads SET website = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_email :exec
UPDATE leads SET email = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_phone :exec
UPDATE leads SET phone = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_contact_person :exec
UPDATE leads SET contact_person = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_contact_title :exec
UPDATE leads SET contact_title = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_city :exec
UPDATE leads SET city = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_state :exec
UPDATE leads SET state = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_address :exec
UPDATE leads SET address = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_specialization :exec
UPDATE leads SET specialization = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_company_size :exec
UPDATE leads SET company_size = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_employee_count_exact :exec
UPDATE leads SET employee_count_exact = sqlc.arg(value)::int
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_description :exec
UPDATE leads SET description = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_revenue_range :exec
UPDATE leads SET revenue_range = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_founded_year :exec
UPDATE leads SET founded_year = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_industry_tags :exec
UPDATE leads SET industry_tags = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_technologies :exec
UPDATE leads SET technologies = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_funding_stage :exec
UPDATE leads SET funding_stage = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_linkedin_url :exec
UPDATE leads SET linkedin_url = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_twitter_url :exec
UPDATE leads SET twitter_url = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_facebook_url :exec
UPDATE leads SET facebook_url = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_secondary_emails :exec
UPDATE leads SET secondary_emails = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_secondary_phones :exec
UPDATE leads SET secondary_phones = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_decision_makers :exec
UPDATE leads SET decision_makers = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_status :exec
UPDATE leads SET status = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;

-- name: DestInboundSetLead_notes :exec
UPDATE leads SET notes = sqlc.arg(value)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::bigint;
