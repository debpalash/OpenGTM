-- Living-workbook refresh (port of services/workbook/refresh.py). Tenant tables
-- run inside tenancy.InWorkspace (RLS is the tenant guard; workbook id explicit).
-- Timestamps are naive UTC columns.

-- name: RefreshWorkbook :one
SELECT workspace_id, columns_config, refresh_policy
FROM workbooks
WHERE id = sqlc.arg(id)::text;

-- name: SetRefreshPolicy :execrows
UPDATE workbooks SET refresh_policy = sqlc.arg(policy)::json
WHERE id = sqlc.arg(id)::text;

-- name: RefreshPageBounds :one
-- Upper id bound and size of the next keyset page of rows.
SELECT COALESCE(max(id), 0)::int4 AS hi, count(*)::int8 AS n
FROM (
    SELECT r.id FROM workbook_rows r
    WHERE r.workbook_id = sqlc.arg(workbook_id)::text
      AND r.id > sqlc.arg(after)::int4
    ORDER BY r.id
    LIMIT sqlc.arg(page_size)::int4
) page;

-- name: RefreshStalePairs :many
-- Exact (row, column) cells that are missing, not complete, undated or older
-- than the column cutoff, for rows in (after, hi]. The overlay key is the
-- linked lead id, else the row id.
SELECT r.id::int4 AS row_id, c.cid::text AS column_id
FROM workbook_rows r
CROSS JOIN LATERAL (
    SELECT (sqlc.arg(cids)::text[])[g] AS cid, g AS ord, (sqlc.arg(cutoffs)::timestamp[])[g] AS cutoff
    FROM generate_series(1, cardinality(sqlc.arg(cids)::text[])) AS g
) c
LEFT JOIN workbook_enrichments e
       ON e.workbook_id = sqlc.arg(workbook_id)::text
      AND e.lead_id = COALESCE(r.lead_id, r.id)
      AND e.column_id = c.cid
WHERE r.workbook_id = sqlc.arg(workbook_id)::text
  AND r.id > sqlc.arg(after)::int4
  AND r.id <= sqlc.arg(hi)::int4
  AND (e.id IS NULL OR e.status IS NULL OR e.status <> 'complete'
       OR e.updated_at IS NULL OR e.updated_at < c.cutoff)
ORDER BY r.id, c.ord;

-- name: InsertRefreshActivity :exec
INSERT INTO workbook_activity (workbook_id, workspace_id, kind, message, created_at)
VALUES (sqlc.arg(workbook_id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(kind)::text,
        sqlc.arg(message)::text, (now() AT TIME ZONE 'UTC'));

-- name: ActiveRefreshJobForWorkbook :one
SELECT id FROM jobs
WHERE type = 'refresh_workbook' AND status IN ('pending', 'processing')
  AND payload->>'workbook_id' = sqlc.arg(workbook_id)::text
LIMIT 1;

-- name: InsertRefreshJobAt :one
-- Delayed, fire-keyed refresh_workbook insert (EnqueueOnce has no run-at).
INSERT INTO jobs (executor, type, payload, workspace_id, priority, fire_key, status,
                  created_at, next_run_at, max_retries, retry_count)
VALUES (sqlc.arg(executor)::text, 'refresh_workbook', sqlc.arg(payload)::json,
        sqlc.arg(workspace_id)::text, 1, sqlc.arg(fire_key)::text, 'pending',
        (now() AT TIME ZONE 'UTC'), sqlc.arg(next_run_at)::timestamp, 3, 0)
ON CONFLICT (fire_key) WHERE status IN ('pending', 'processing') DO NOTHING
RETURNING id;

-- name: OtherActiveRefreshJob :one
-- Another refresh_workbook that will chain instead of this job: any pending one,
-- or a processing one with a lower id (the lowest id always chains).
SELECT id FROM jobs
WHERE type = 'refresh_workbook' AND status IN ('pending', 'processing')
  AND payload->>'workbook_id' = sqlc.arg(workbook_id)::text
  AND id <> sqlc.arg(self_id)::int4
  AND (status = 'pending' OR id < sqlc.arg(self_id)::int4)
LIMIT 1;
