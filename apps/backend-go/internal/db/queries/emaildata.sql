-- Queries for plan 07 Task 14: the /api/data email data service.
-- email_data and links are legacy GLOBAL tables (no workspace_id, no RLS); they are
-- reached only through the global-admin gate. Filters are NULL-able: a NULL argument
-- means "not filtered" (Python skips falsy filters; the Go service maps 0/empty to NULL).
-- The ORDER BY allow-list is a CASE per column and direction; unknown names never
-- reach SQL (the service falls back to id). id ASC is the deterministic tie-break.

-- name: ListEmailData :many
SELECT e.id, e.name, e.email, e.source_link_id, e.created_at, e.is_used, e.tags, e.notes, l.url AS source_url
FROM email_data e
LEFT JOIN links l ON e.source_link_id = l.id
WHERE (sqlc.narg('source_link_id')::int IS NULL OR e.source_link_id = sqlc.narg('source_link_id')::int)
  AND (sqlc.narg('q')::text IS NULL
       OR e.name ILIKE '%' || replace(replace(replace(sqlc.narg('q')::text, '\', '\\'), '%', '\%'), '_', '\_') || '%' ESCAPE '\'
       OR e.email ILIKE '%' || replace(replace(replace(sqlc.narg('q')::text, '\', '\\'), '%', '\%'), '_', '\_') || '%' ESCAPE '\')
  AND (sqlc.narg('start_date')::text IS NULL OR e.created_at >= sqlc.narg('start_date')::text)
  AND (sqlc.narg('end_date')::text IS NULL OR e.created_at <= sqlc.narg('end_date')::text)
  AND (sqlc.narg('is_used')::bool IS NULL OR e.is_used = sqlc.narg('is_used')::bool)
ORDER BY
  CASE WHEN sqlc.arg('sort_by')::text = 'id' AND NOT sqlc.arg('sort_desc')::bool THEN e.id END ASC,
  CASE WHEN sqlc.arg('sort_by')::text = 'id' AND sqlc.arg('sort_desc')::bool THEN e.id END DESC,
  CASE WHEN sqlc.arg('sort_by')::text = 'name' AND NOT sqlc.arg('sort_desc')::bool THEN e.name END ASC,
  CASE WHEN sqlc.arg('sort_by')::text = 'name' AND sqlc.arg('sort_desc')::bool THEN e.name END DESC,
  CASE WHEN sqlc.arg('sort_by')::text = 'email' AND NOT sqlc.arg('sort_desc')::bool THEN e.email END ASC,
  CASE WHEN sqlc.arg('sort_by')::text = 'email' AND sqlc.arg('sort_desc')::bool THEN e.email END DESC,
  CASE WHEN sqlc.arg('sort_by')::text = 'source_link_id' AND NOT sqlc.arg('sort_desc')::bool THEN e.source_link_id END ASC,
  CASE WHEN sqlc.arg('sort_by')::text = 'source_link_id' AND sqlc.arg('sort_desc')::bool THEN e.source_link_id END DESC,
  CASE WHEN sqlc.arg('sort_by')::text = 'created_at' AND NOT sqlc.arg('sort_desc')::bool THEN e.created_at END ASC,
  CASE WHEN sqlc.arg('sort_by')::text = 'created_at' AND sqlc.arg('sort_desc')::bool THEN e.created_at END DESC,
  CASE WHEN sqlc.arg('sort_by')::text = 'is_used' AND NOT sqlc.arg('sort_desc')::bool THEN e.is_used END ASC,
  CASE WHEN sqlc.arg('sort_by')::text = 'is_used' AND sqlc.arg('sort_desc')::bool THEN e.is_used END DESC,
  CASE WHEN sqlc.arg('sort_by')::text = 'tags' AND NOT sqlc.arg('sort_desc')::bool THEN e.tags END ASC,
  CASE WHEN sqlc.arg('sort_by')::text = 'tags' AND sqlc.arg('sort_desc')::bool THEN e.tags END DESC,
  CASE WHEN sqlc.arg('sort_by')::text = 'notes' AND NOT sqlc.arg('sort_desc')::bool THEN e.notes END ASC,
  CASE WHEN sqlc.arg('sort_by')::text = 'notes' AND sqlc.arg('sort_desc')::bool THEN e.notes END DESC,
  e.id ASC
LIMIT sqlc.arg('row_limit')::bigint OFFSET sqlc.arg('row_offset')::bigint;

-- name: CountEmailData :one
SELECT count(*) FROM email_data e
WHERE (sqlc.narg('source_link_id')::int IS NULL OR e.source_link_id = sqlc.narg('source_link_id')::int)
  AND (sqlc.narg('q')::text IS NULL
       OR e.name ILIKE '%' || replace(replace(replace(sqlc.narg('q')::text, '\', '\\'), '%', '\%'), '_', '\_') || '%' ESCAPE '\'
       OR e.email ILIKE '%' || replace(replace(replace(sqlc.narg('q')::text, '\', '\\'), '%', '\%'), '_', '\_') || '%' ESCAPE '\')
  AND (sqlc.narg('start_date')::text IS NULL OR e.created_at >= sqlc.narg('start_date')::text)
  AND (sqlc.narg('end_date')::text IS NULL OR e.created_at <= sqlc.narg('end_date')::text)
  AND (sqlc.narg('is_used')::bool IS NULL OR e.is_used = sqlc.narg('is_used')::bool);

-- name: ExportEmailData :many
SELECT e.id, e.name, e.email, e.source_link_id, e.created_at, e.is_used, e.tags, e.notes
FROM email_data e
WHERE (sqlc.narg('source_link_id')::int IS NULL OR e.source_link_id = sqlc.narg('source_link_id')::int)
  AND (sqlc.narg('q')::text IS NULL
       OR e.name ILIKE '%' || replace(replace(replace(sqlc.narg('q')::text, '\', '\\'), '%', '\%'), '_', '\_') || '%' ESCAPE '\'
       OR e.email ILIKE '%' || replace(replace(replace(sqlc.narg('q')::text, '\', '\\'), '%', '\%'), '_', '\_') || '%' ESCAPE '\')
  AND (sqlc.narg('start_date')::text IS NULL OR e.created_at >= sqlc.narg('start_date')::text)
  AND (sqlc.narg('end_date')::text IS NULL OR e.created_at <= sqlc.narg('end_date')::text)
  AND (sqlc.narg('is_used')::bool IS NULL OR e.is_used = sqlc.narg('is_used')::bool)
ORDER BY
  CASE WHEN sqlc.arg('sort_by')::text = 'id' AND NOT sqlc.arg('sort_desc')::bool THEN e.id END ASC,
  CASE WHEN sqlc.arg('sort_by')::text = 'id' AND sqlc.arg('sort_desc')::bool THEN e.id END DESC,
  CASE WHEN sqlc.arg('sort_by')::text = 'name' AND NOT sqlc.arg('sort_desc')::bool THEN e.name END ASC,
  CASE WHEN sqlc.arg('sort_by')::text = 'name' AND sqlc.arg('sort_desc')::bool THEN e.name END DESC,
  CASE WHEN sqlc.arg('sort_by')::text = 'email' AND NOT sqlc.arg('sort_desc')::bool THEN e.email END ASC,
  CASE WHEN sqlc.arg('sort_by')::text = 'email' AND sqlc.arg('sort_desc')::bool THEN e.email END DESC,
  CASE WHEN sqlc.arg('sort_by')::text = 'source_link_id' AND NOT sqlc.arg('sort_desc')::bool THEN e.source_link_id END ASC,
  CASE WHEN sqlc.arg('sort_by')::text = 'source_link_id' AND sqlc.arg('sort_desc')::bool THEN e.source_link_id END DESC,
  CASE WHEN sqlc.arg('sort_by')::text = 'created_at' AND NOT sqlc.arg('sort_desc')::bool THEN e.created_at END ASC,
  CASE WHEN sqlc.arg('sort_by')::text = 'created_at' AND sqlc.arg('sort_desc')::bool THEN e.created_at END DESC,
  CASE WHEN sqlc.arg('sort_by')::text = 'is_used' AND NOT sqlc.arg('sort_desc')::bool THEN e.is_used END ASC,
  CASE WHEN sqlc.arg('sort_by')::text = 'is_used' AND sqlc.arg('sort_desc')::bool THEN e.is_used END DESC,
  CASE WHEN sqlc.arg('sort_by')::text = 'tags' AND NOT sqlc.arg('sort_desc')::bool THEN e.tags END ASC,
  CASE WHEN sqlc.arg('sort_by')::text = 'tags' AND sqlc.arg('sort_desc')::bool THEN e.tags END DESC,
  CASE WHEN sqlc.arg('sort_by')::text = 'notes' AND NOT sqlc.arg('sort_desc')::bool THEN e.notes END ASC,
  CASE WHEN sqlc.arg('sort_by')::text = 'notes' AND sqlc.arg('sort_desc')::bool THEN e.notes END DESC,
  e.id ASC
LIMIT sqlc.arg('row_limit')::bigint OFFSET sqlc.arg('row_offset')::bigint;

-- name: MarkEmailDataUsedByIDs :execrows
UPDATE email_data SET is_used = sqlc.arg('used')::bool
WHERE id = ANY(sqlc.arg('ids')::int[]);

-- name: MarkEmailDataUsedByFilter :execrows
UPDATE email_data e SET is_used = sqlc.arg('used')::bool
WHERE (sqlc.narg('source_link_id')::int IS NULL OR e.source_link_id = sqlc.narg('source_link_id')::int)
  AND (sqlc.narg('q')::text IS NULL
       OR e.name ILIKE '%' || replace(replace(replace(sqlc.narg('q')::text, '\', '\\'), '%', '\%'), '_', '\_') || '%' ESCAPE '\'
       OR e.email ILIKE '%' || replace(replace(replace(sqlc.narg('q')::text, '\', '\\'), '%', '\%'), '_', '\_') || '%' ESCAPE '\')
  AND (sqlc.narg('start_date')::text IS NULL OR e.created_at >= sqlc.narg('start_date')::text)
  AND (sqlc.narg('end_date')::text IS NULL OR e.created_at <= sqlc.narg('end_date')::text)
  AND (sqlc.narg('is_used')::bool IS NULL OR e.is_used = sqlc.narg('is_used')::bool);

-- name: UpdateEmailData :one
UPDATE email_data SET
  name = CASE WHEN sqlc.arg('set_name')::bool THEN sqlc.narg('name')::text ELSE name END,
  email = CASE WHEN sqlc.arg('set_email')::bool THEN sqlc.narg('email')::text ELSE email END,
  tags = CASE WHEN sqlc.arg('set_tags')::bool THEN sqlc.narg('tags')::text ELSE tags END,
  notes = CASE WHEN sqlc.arg('set_notes')::bool THEN sqlc.narg('notes')::text ELSE notes END,
  is_used = CASE WHEN sqlc.arg('set_is_used')::bool THEN sqlc.narg('is_used')::bool ELSE is_used END
WHERE id = sqlc.arg('id')::int
RETURNING id, name, email, source_link_id, created_at, is_used, tags, notes;

-- name: DeleteEmailData :execrows
DELETE FROM email_data WHERE id = sqlc.arg('id')::int;

-- name: EmailDataTimeline :many
SELECT left(created_at, 10)::text AS day, count(*)::bigint AS n
FROM email_data
WHERE created_at IS NOT NULL AND created_at <> ''
GROUP BY 1;

-- name: EmailDataEmails :many
SELECT email::text FROM email_data
WHERE email IS NOT NULL AND email LIKE '%@%'
ORDER BY id;
