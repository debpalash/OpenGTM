-- CSV import writes (wbcrud importsvc). Every statement runs inside
-- tenancy.InWorkspace (FORCE RLS) and also carries the explicit
-- workbook/workspace predicate. The workbook UPDATE is an ORM UPDATE in Python,
-- so it stamps updated_at = now() at time zone 'utc'.

-- name: ImportMaxRowPosition :one
-- max(position) or -1 when there is none: new rows start at that value + 1,
-- so an empty workbook starts at 0 (import_csv_leads: next_pos = 0 if max is None).
SELECT coalesce(max("position"), -1)::int AS max_position
FROM workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text;

-- name: ImportAppendRows :many
-- One row per element of datas (JSON text), positions start_position+1.. in
-- order. corroboration_count is the ORM column default (1); enrichments starts
-- empty; lead_id stays NULL (import_csv_leads never sets it).
INSERT INTO workbook_rows (workspace_id, workbook_id, "position", data, enrichments, corroboration_count)
SELECT sqlc.arg(workspace_id)::text, sqlc.arg(workbook_id)::text, sqlc.arg(start_position)::int + ord::int,
       d::json, '{}'::json, 1
FROM unnest(sqlc.arg(datas)::text[]) WITH ORDINALITY AS x(d, ord)
WHERE EXISTS (SELECT 1 FROM workbooks wb WHERE wb.id = sqlc.arg(workbook_id)::text AND wb.workspace_id = sqlc.arg(workspace_id)::text)
ORDER BY ord
RETURNING id;

-- name: ImportUpdateWorkbook :exec
-- columns_config, source_type and source_config of an import (always a real
-- change: source_config carries imported_at), updated_at stamped.
UPDATE workbooks
SET columns_config = sqlc.arg(columns_config)::json,
    source_type = sqlc.arg(source_type)::text,
    source_config = sqlc.arg(source_config)::json,
    updated_at = (now() AT TIME ZONE 'utc')
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;
