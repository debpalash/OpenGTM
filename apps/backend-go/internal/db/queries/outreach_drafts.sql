-- plan 09 (outreach): grounded draft read routes. Read-only; each statement runs inside tenancy.InWorkspace
-- and still filters workspace_id (belt and suspenders with FORCE RLS).

-- name: OutreachDraftList :many
SELECT id, person_id, person_name, company, title, to_email, contact_status, risky_approved, generic_inbox,
       is_role_address, subject, body_text, state, sent_at, created_at, updated_at
FROM outreach_drafts
WHERE workspace_id = sqlc.arg(workspace_id)::text
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::bigint OFFSET sqlc.arg(row_offset)::bigint;

-- name: OutreachDraftGet :one
SELECT id, person_id, person_name, company, title, to_email, contact_status, risky_approved, generic_inbox,
       is_role_address, subject, body_text, state, sent_at, created_at, updated_at,
       sentence_evidence::text AS sentence_evidence, source_snapshot::text AS source_snapshot
FROM outreach_drafts
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text;
