-- Workbook v1 -> v2 migration (Task 28, POST /{id}/migrate). Every statement runs
-- inside tenancy.InWorkspace (FORCE RLS) and also carries the explicit workspace_id.

-- name: MigrateInsertRows :exec
-- Bulk snapshot insert from one jsonb array of {"p": position, "l": lead id or 0,
-- "d": row data as a JSON *string*, "e": enrichment overlay as a JSON *string*}
-- (the strings keep the json columns' key order, which jsonb would not). corroboration_count
-- mirrors the ORM default (1); timestamps use the table defaults.
INSERT INTO workbook_rows (workbook_id, workspace_id, position, data, enrichments, lead_id, corroboration_count)
SELECT sqlc.arg(workbook_id)::text, sqlc.arg(workspace_id)::text, (e.value->>'p')::int,
       (e.value->>'d')::json, (e.value->>'e')::json, NULLIF((e.value->>'l')::int, 0), 1
FROM jsonb_array_elements(sqlc.arg(items)::jsonb) WITH ORDINALITY AS e(value, ord)
ORDER BY e.ord;

-- name: MigrateCountEnrichments :one
SELECT COUNT(*)::bigint FROM workbook_enrichments
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: MigrateEnrichmentsForLeads :many
-- The v1 overlay rows of one page of leads, in insertion order (a later duplicate of a
-- column overwrites the earlier one, like the Python dict build).
SELECT lead_id::int AS lead_id, column_id, value, status, provider, error
FROM workbook_enrichments
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
  AND lead_id = ANY(sqlc.arg(lead_ids)::int[])
ORDER BY id;

-- name: UpdateWorkbookSource :one
-- Sets source_type and source_config; the caller (UpdateIfChanged) already decided
-- that one of them really changed, so updated_at is stamped.
UPDATE workbooks SET
  source_type = COALESCE(sqlc.narg(source_type)::text, source_type),
  source_config = CASE WHEN sqlc.arg(set_source_config)::bool THEN sqlc.narg(source_config)::json ELSE source_config END,
  updated_at = (now() AT TIME ZONE 'utc')
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
RETURNING workbooks.*;
