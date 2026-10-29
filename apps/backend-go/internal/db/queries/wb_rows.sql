-- Workbook row writes for the row routes (wbcrud rowsvc). Every statement runs
-- inside tenancy.InWorkspace (FORCE RLS) and also carries the explicit
-- workbook/workspace predicate. Every UPDATE that SQLAlchemy stamps with
-- onupdate=func.now() sets updated_at = now() at time zone 'utc'.

-- name: MaxWorkbookRowPosition :one
-- `max(position) or 0` of add_rows.
SELECT coalesce(max("position"), 0)::int AS max_position
FROM workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text;

-- name: AppendWorkbookRows :many
-- One row per element of datas (JSON text), positions start_position+1.. in
-- order. lead_ids holds the decimal lead id or '' for NULL. corroboration_count
-- is the ORM column default (1); enrichments starts empty.
INSERT INTO workbook_rows (workspace_id, workbook_id, "position", data, enrichments, lead_id, corroboration_count)
SELECT sqlc.arg(workspace_id)::text, sqlc.arg(workbook_id)::text, sqlc.arg(start_position)::int + ord::int,
       d::json, '{}'::json, NULLIF(l, '')::int, 1
FROM (SELECT v AS d, ord FROM unnest(sqlc.arg(datas)::text[]) WITH ORDINALITY AS x(v, ord)) AS a
JOIN (SELECT v AS l, ord FROM unnest(sqlc.arg(lead_ids)::text[]) WITH ORDINALITY AS y(v, ord)) AS b USING (ord)
WHERE EXISTS (SELECT 1 FROM workbooks wb WHERE wb.id = sqlc.arg(workbook_id)::text AND wb.workspace_id = sqlc.arg(workspace_id)::text)
ORDER BY ord
RETURNING id;

-- name: DeleteWorkbookRowsByIDs :execrows
DELETE FROM workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text AND id = ANY(sqlc.arg(row_ids)::bigint[]);

-- name: UpdateWorkbookSourceConfig :exec
-- mark_row_storage: the caller already decided the value really changed.
UPDATE workbooks SET source_config = sqlc.arg(source_config)::json, updated_at = (now() AT TIME ZONE 'utc')
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: LockWorkbookRows :many
-- Rows of one workbook by id, locked in id order (no lost updates, no deadlocks
-- between concurrent bulk patches). Missing / foreign ids are simply absent.
SELECT id, data FROM workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text AND id = ANY(sqlc.arg(row_ids)::bigint[])
ORDER BY id
FOR UPDATE;

-- name: PatchWorkbookRowsData :execrows
-- Set data for several rows of one workbook in a single statement.
UPDATE workbook_rows AS r
SET data = u.d::json, updated_at = (now() AT TIME ZONE 'utc')
FROM (SELECT a.id, b.d FROM
        (SELECT v AS id, ord FROM unnest(sqlc.arg(row_ids)::bigint[]) WITH ORDINALITY AS x(v, ord)) AS a
        JOIN (SELECT v AS d, ord FROM unnest(sqlc.arg(datas)::text[]) WITH ORDINALITY AS y(v, ord)) AS b USING (ord)) AS u
WHERE r.workbook_id = sqlc.arg(workbook_id)::text AND r.id = u.id;

-- name: ExistingWorkbookRowIDs :many
SELECT id FROM workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text AND id = ANY(sqlc.arg(row_ids)::bigint[])
ORDER BY id;

-- name: StartWorkbookRun :exec
-- run_workbook: status running, progress reset (ORM UPDATE, so updated_at is stamped).
UPDATE workbooks SET status = 'running', last_run_at = (now() AT TIME ZONE 'utc'),
  total_rows = sqlc.arg(total_rows)::int, completed_rows = 0, updated_at = (now() AT TIME ZONE 'utc')
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: CancelActiveRunJobs :exec
-- A new run supersedes any in-flight run_workbook job of the same workbook.
UPDATE jobs SET status = 'cancelled', error = 'superseded by a newer run'
WHERE type = 'run_workbook' AND status IN ('pending', 'processing')
  AND payload->>'workbook_id' = sqlc.arg(workbook_id)::text;
