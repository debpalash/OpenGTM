-- Source ingest upsert (plan 07 task 8). Every statement runs inside
-- tenancy.InWorkspace (FORCE RLS) and also carries the explicit
-- workbook/workspace predicate.

-- name: SourceIngestWorkbookExists :one
SELECT EXISTS (SELECT 1 FROM workbooks WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text) AS present;

-- name: SourceIngestLock :exec
-- lock_source_ingest: serialises one provider's page commits for a workbook.
SELECT pg_advisory_xact_lock(hashtext(sqlc.arg(lock_key)::text));

-- name: SourceIngestDeleteRows :exec
DELETE FROM workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: SourceIngestExistingRows :many
SELECT id, source_record_id::text AS source_record_id, data::text AS data, source_rank, source_fetched_at
FROM record_workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
  AND source_provider = sqlc.arg(provider)::text AND source_record_id IS NOT NULL;

-- name: SourceIngestMaxPosition :one
-- max(position), -1 when there is none (next row = value + 1).
SELECT coalesce(max("position"), -1)::int AS max_position
FROM record_workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: SourceIngestInsertRow :one
-- corroboration_count is the ORM column default (1); lead_id stays NULL.
INSERT INTO workbook_rows (workspace_id, workbook_id, "position", data, enrichments, lead_id, corroboration_count,
                           source_provider, source_record_id, source_rank, source_fetched_at)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(workbook_id)::text, sqlc.arg(position)::int, sqlc.arg(data)::text::json, '{}'::json, NULL, 1,
        sqlc.arg(provider)::text, sqlc.arg(record_id)::text, sqlc.arg(rank)::int, sqlc.arg(fetched_at)::timestamptz)
RETURNING id;

-- name: SourceIngestUpdateRow :exec
-- A NULL position keeps the row's position (position is only rewritten on reset).
UPDATE workbook_rows
SET data = sqlc.arg(data)::text::json,
    source_rank = sqlc.arg(rank)::int,
    source_fetched_at = sqlc.arg(fetched_at)::timestamptz,
    "position" = coalesce(sqlc.narg(position)::int, "position"),
    updated_at = (now() AT TIME ZONE 'utc')
WHERE id = sqlc.arg(id)::int AND workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;
