-- Entity resolution (plan 15 WP-SOURCEWB): ports of entities/graph.py resolve_company and
-- entities/people.py resolve_person. Every statement runs inside tenancy.InWorkspace (FORCE RLS)
-- and still passes workspace_id explicitly. The unique constraints uq_company_identifier and
-- uq_person_identifier / uq_person_employment make concurrent resolutions converge: an insert that
-- loses the race does nothing and the caller re-reads the owner (Python: savepoint + IntegrityError).

-- name: CompanyIdentifierOwner :one
SELECT entity_id FROM company_identifiers
WHERE workspace_id = sqlc.arg(workspace_id)::text AND kind = sqlc.arg(kind)::text AND value = sqlc.arg(value)::text
LIMIT 1;

-- name: InsertCompanyIdentifier :execrows
INSERT INTO company_identifiers (workspace_id, kind, value, entity_id, created_at)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(kind)::text, sqlc.arg(value)::text, sqlc.arg(entity_id)::text,
        (now() at time zone 'utc'))
ON CONFLICT ON CONSTRAINT uq_company_identifier DO NOTHING;

-- name: BlockingCandidateIDs :many
SELECT DISTINCT entity_id FROM entity_blocking_keys
WHERE workspace_id = sqlc.arg(workspace_id)::text AND key = ANY(sqlc.arg(keys)::text[])
ORDER BY entity_id;

-- name: InsertEntityReviewPair :exec
INSERT INTO entity_review_pairs (workspace_id, entity_id, candidate, score, reason, status, created_at)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(entity_id)::text, sqlc.arg(candidate)::json,
        sqlc.arg(score)::float8, sqlc.arg(reason)::text, 'pending', (now() at time zone 'utc'));

-- name: PersonIdentifierOwner :one
SELECT person_id FROM person_identifiers
WHERE workspace_id = sqlc.arg(workspace_id)::text AND kind = sqlc.arg(kind)::text AND value = sqlc.arg(value)::text
LIMIT 1;

-- name: InsertPersonIdentifier :execrows
INSERT INTO person_identifiers (workspace_id, kind, value, person_id, created_at)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(kind)::text, sqlc.arg(value)::text, sqlc.arg(person_id)::text,
        (now() at time zone 'utc'))
ON CONFLICT ON CONSTRAINT uq_person_identifier DO NOTHING;

-- name: PersonLinkedInValues :many
SELECT value FROM person_identifiers
WHERE workspace_id = sqlc.arg(workspace_id)::text AND person_id = sqlc.arg(person_id)::text AND kind = 'linkedin'
ORDER BY id;

-- name: PersonEntityExists :one
SELECT EXISTS (SELECT 1 FROM person_entities
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text);

-- name: InsertPersonEntity :exec
INSERT INTO person_entities (id, workspace_id, full_name, identity_keys, fields, corroboration_count)
VALUES (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(full_name)::text, '{}', '{}', 0);

-- name: LockPersonEntity :one
SELECT id, full_name, identity_keys, fields FROM person_entities
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
FOR UPDATE;

-- name: UpdatePersonEntity :exec
UPDATE person_entities SET
  full_name = sqlc.arg(full_name)::text,
  identity_keys = sqlc.arg(identity_keys)::json,
  fields = sqlc.arg(fields)::json,
  corroboration_count = sqlc.arg(corroboration_count)::int,
  company_entity_id = sqlc.narg(company_entity_id)::text,
  last_seen = (now() at time zone 'utc')
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: PersonEmploymentByKey :one
SELECT id, company_entity_id, title, titles, evidence_url, first_observed_at, last_observed_at
FROM person_employments
WHERE workspace_id = sqlc.arg(workspace_id)::text AND person_id = sqlc.arg(person_id)::text
  AND company_key = sqlc.arg(company_key)::text
FOR UPDATE;

-- name: InsertPersonEmployment :execrows
INSERT INTO person_employments (workspace_id, person_id, company_key, company_entity_id, company_name, company_domain,
  title, titles, source, evidence_url, first_observed_at, last_observed_at, is_current)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(person_id)::text, sqlc.arg(company_key)::text,
  sqlc.narg(company_entity_id)::text, sqlc.arg(company_name)::text, sqlc.arg(company_domain)::text,
  sqlc.arg(title)::text, sqlc.arg(titles)::json, sqlc.arg(source)::text, sqlc.arg(evidence_url)::text,
  sqlc.arg(observed_at)::timestamp, sqlc.arg(observed_at)::timestamp, 1)
ON CONFLICT ON CONSTRAINT uq_person_employment DO NOTHING;

-- name: UpdatePersonEmployment :exec
UPDATE person_employments SET
  company_entity_id = sqlc.narg(company_entity_id)::text,
  title = sqlc.narg(title)::text,
  titles = sqlc.narg(titles)::json,
  evidence_url = sqlc.narg(evidence_url)::text,
  first_observed_at = sqlc.arg(first_observed_at)::timestamp,
  last_observed_at = sqlc.arg(last_observed_at)::timestamp
WHERE id = sqlc.arg(id)::int AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: MarkCurrentEmployment :one
-- The most recently observed employment is current, older ones become history; returns the
-- current one's company entity (person.company_entity_id).
WITH ranked AS (
  SELECT id, company_entity_id, row_number() OVER (ORDER BY last_observed_at DESC, id DESC) AS rn
  FROM person_employments
  WHERE workspace_id = sqlc.arg(workspace_id)::text AND person_id = sqlc.arg(person_id)::text
), upd AS (
  UPDATE person_employments e SET is_current = CASE WHEN r.rn = 1 THEN 1 ELSE 0 END
  FROM ranked r WHERE e.id = r.id
)
SELECT company_entity_id FROM ranked WHERE rn = 1;
