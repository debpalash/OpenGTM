-- Entity merge/split/decide writes (Task 17). Every statement runs inside tenancy.InWorkspace
-- (FORCE RLS on all tables touched) and still passes workspace_id explicitly.
-- Python stamps updated_at/last_seen through onupdate=func.now(): the UPDATEs below do the same
-- with the naive UTC clock.

-- name: LockCompanyEntity :one
SELECT * FROM company_entities
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
FOR UPDATE;

-- name: ListEntityBlockingKeys :many
SELECT key FROM entity_blocking_keys
WHERE entity_id = sqlc.arg(entity_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
ORDER BY id;

-- name: InsertEntityBlockingKey :exec
INSERT INTO entity_blocking_keys (key, entity_id, workspace_id)
VALUES (sqlc.arg(key)::text, sqlc.arg(entity_id)::text, sqlc.arg(workspace_id)::text);

-- name: DeleteEntityBlockingKeys :exec
DELETE FROM entity_blocking_keys
WHERE entity_id = sqlc.arg(entity_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: ListEntityRowIDs :many
SELECT id FROM workbook_rows
WHERE canonical_entity_id = sqlc.arg(entity_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
ORDER BY id;

-- name: RepointWorkbookRows :exec
UPDATE workbook_rows SET canonical_entity_id = sqlc.arg(kept_id)::text, updated_at = (now() at time zone 'utc')
WHERE canonical_entity_id = sqlc.arg(merged_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: RebindWorkbookRow :exec
UPDATE workbook_rows SET canonical_entity_id = sqlc.arg(entity_id)::text, updated_at = (now() at time zone 'utc')
WHERE id = sqlc.arg(id)::int AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: ListAccountReferenceRows :many
SELECT id, data FROM workbook_rows
WHERE workspace_id = sqlc.arg(workspace_id)::text AND strpos(data::text, sqlc.arg(entity_id)::text) > 0
ORDER BY id
FOR UPDATE;

-- name: GetWorkbookRowData :one
SELECT data FROM workbook_rows
WHERE id = sqlc.arg(id)::int AND workspace_id = sqlc.arg(workspace_id)::text
FOR UPDATE;

-- name: UpdateWorkbookRowData :exec
UPDATE workbook_rows SET data = sqlc.arg(data)::json, updated_at = (now() at time zone 'utc')
WHERE id = sqlc.arg(id)::int AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: ListEntityIdentifiers :many
SELECT kind, value FROM company_identifiers
WHERE entity_id = sqlc.arg(entity_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
ORDER BY id;

-- name: RepointCompanyIdentifiers :exec
UPDATE company_identifiers SET entity_id = sqlc.arg(kept_id)::text
WHERE entity_id = sqlc.arg(merged_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: RestoreCompanyIdentifier :exec
UPDATE company_identifiers SET entity_id = sqlc.arg(entity_id)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND kind = sqlc.arg(kind)::text AND value = sqlc.arg(value)::text;

-- name: ListAccountGroupWatches :many
SELECT id, config, cursor FROM watch_subscriptions
WHERE workspace_id = sqlc.arg(workspace_id)::text AND kind = 'account_group'
ORDER BY id
FOR UPDATE;

-- name: GetWatchForRestore :one
SELECT id, config, cursor FROM watch_subscriptions
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
FOR UPDATE;

-- name: UpdateWatchConfigCursor :exec
UPDATE watch_subscriptions SET config = sqlc.arg(config)::json, cursor = sqlc.arg(cursor)::json
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: ListCompanyPersonIDs :many
SELECT id FROM person_entities
WHERE workspace_id = sqlc.arg(workspace_id)::text AND company_entity_id = sqlc.arg(company_id)::text
ORDER BY id;

-- name: ListCompanyEmploymentIDs :many
SELECT id FROM person_employments
WHERE workspace_id = sqlc.arg(workspace_id)::text AND company_entity_id = sqlc.arg(company_id)::text
ORDER BY id;

-- name: RepointPersonEntities :exec
UPDATE person_entities SET company_entity_id = sqlc.arg(kept_id)::text, last_seen = (now() at time zone 'utc')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND company_entity_id = sqlc.arg(merged_id)::text;

-- name: RepointPersonEmployments :exec
UPDATE person_employments SET company_entity_id = sqlc.arg(kept_id)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND company_entity_id = sqlc.arg(merged_id)::text;

-- name: RestorePersonEntities :exec
UPDATE person_entities SET company_entity_id = sqlc.arg(merged_id)::text, last_seen = (now() at time zone 'utc')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = ANY(sqlc.arg(ids)::text[])
  AND company_entity_id = sqlc.arg(kept_id)::text;

-- name: RestorePersonEmployments :exec
UPDATE person_employments SET company_entity_id = sqlc.arg(merged_id)::text
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = ANY(sqlc.arg(ids)::int[])
  AND company_entity_id = sqlc.arg(kept_id)::text;

-- name: InsertCompanyEntity :exec
INSERT INTO company_entities (id, workspace_id, canonical_name, primary_domain, primary_phone, primary_email,
  primary_city, identity_keys, fields, sources, source_agreement, corroboration_count, observation_count)
VALUES (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(canonical_name)::text,
  sqlc.narg(primary_domain)::text, sqlc.narg(primary_phone)::text, sqlc.narg(primary_email)::text,
  sqlc.narg(primary_city)::text, sqlc.narg(identity_keys)::json, sqlc.narg(fields)::json,
  sqlc.narg(sources)::json, sqlc.narg(source_agreement)::json, sqlc.narg(corroboration_count)::int,
  sqlc.narg(observation_count)::int);

-- name: UpdateCompanyEntityState :exec
UPDATE company_entities SET
  canonical_name = sqlc.arg(canonical_name)::text,
  primary_domain = sqlc.narg(primary_domain)::text,
  primary_phone = sqlc.narg(primary_phone)::text,
  primary_email = sqlc.narg(primary_email)::text,
  primary_city = sqlc.narg(primary_city)::text,
  identity_keys = sqlc.narg(identity_keys)::json,
  fields = sqlc.narg(fields)::json,
  sources = sqlc.narg(sources)::json,
  source_agreement = sqlc.narg(source_agreement)::json,
  corroboration_count = sqlc.narg(corroboration_count)::int,
  observation_count = sqlc.narg(observation_count)::int,
  last_seen = (now() at time zone 'utc')
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: DeleteCompanyEntity :exec
DELETE FROM company_entities
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: InsertEntityMergeLog :one
INSERT INTO entity_merge_log (workspace_id, kept_id, merged_id, reason, score, snapshot, reverted)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(kept_id)::text, sqlc.arg(merged_id)::text,
  sqlc.arg(reason)::text, sqlc.arg(score)::float8, sqlc.arg(snapshot)::json, 0)
RETURNING id;

-- name: LockEntityMergeLog :one
SELECT * FROM entity_merge_log
WHERE id = sqlc.arg(id)::int AND workspace_id = sqlc.arg(workspace_id)::text
FOR UPDATE;

-- name: MarkEntityMergeLogReverted :exec
UPDATE entity_merge_log SET reverted = 1
WHERE id = sqlc.arg(id)::int AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: LockEntityReviewPair :one
SELECT p.id, p.entity_id, p.candidate
FROM entity_review_pairs p
JOIN company_entities e ON e.id = p.entity_id
WHERE p.id = sqlc.arg(id)::int AND e.workspace_id = sqlc.arg(workspace_id)::text
FOR UPDATE OF p;

-- name: SetEntityReviewPairStatus :exec
UPDATE entity_review_pairs SET status = sqlc.arg(status)::text WHERE id = sqlc.arg(id)::int;
