-- source_workbook (plan 15 WP-SOURCEWB): port of workbook/source_engine.py, crm_import.py and
-- people_search.py. workbooks and workbook_rows are FORCE RLS: every statement runs inside
-- tenancy.InWorkspace and still filters on workspace_id.

-- name: SWLoadWorkbook :one
SELECT columns_config, source_config FROM workbooks
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: SWSetStatus :exec
UPDATE workbooks SET status = sqlc.arg(status)::text, updated_at = (now() at time zone 'utc')
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: SWResetRunning :exec
UPDATE workbooks SET status = 'draft', updated_at = (now() at time zone 'utc')
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text AND status = 'running';

-- name: SWFireKeyUsed :one
SELECT EXISTS (SELECT 1 FROM jobs WHERE fire_key = sqlc.arg(fire_key)::text AND workspace_id = sqlc.arg(workspace_id)::text)::bool AS used;

-- name: SWLockWorkbook :one
SELECT source_config FROM workbooks
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
FOR UPDATE;

-- name: SWFinishWorkbook :exec
-- Sourcing done: total_rows recounted, status back to draft, optional source_config.
UPDATE workbooks SET
  total_rows = (SELECT count(*) FROM workbook_rows r
                WHERE r.workbook_id = sqlc.arg(id)::text AND r.workspace_id = sqlc.arg(workspace_id)::text),
  status = 'draft',
  source_config = coalesce(sqlc.narg(source_config)::json, source_config),
  updated_at = (now() at time zone 'utc')
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: SWEntityIDs :many
SELECT DISTINCT canonical_entity_id::text FROM workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
  AND canonical_entity_id IS NOT NULL;

-- name: SWRowStats :one
SELECT count(*)::bigint AS n, coalesce(max(position), 0)::int AS max_pos FROM workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: SWCountRows :one
SELECT count(*)::bigint FROM workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: SWRowsData :many
SELECT id, lead_id, data, enrichments FROM workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
ORDER BY position ASC NULLS LAST, id ASC;

-- name: SWInsertRow :one
INSERT INTO workbook_rows (workbook_id, workspace_id, position, data, enrichments, lead_id, canonical_entity_id,
  corroboration_count, source_provider, source_record_id, created_at, updated_at)
VALUES (sqlc.arg(workbook_id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(position)::int, sqlc.arg(data)::json, '{}',
  sqlc.narg(lead_id)::int, sqlc.narg(canonical_entity_id)::text, sqlc.narg(corroboration_count)::int,
  sqlc.narg(source_provider)::text, sqlc.narg(source_record_id)::text,
  (now() at time zone 'utc'), (now() at time zone 'utc'))
RETURNING id;

-- name: SWEnqueueJob :one
-- source_workbook insert with an explicit executor (queue_service.add_job shape otherwise:
-- priority 1, max_retries 3, no fire key).
INSERT INTO jobs (executor, type, payload, workspace_id, priority, status,
                  created_at, next_run_at, max_retries, retry_count)
VALUES (sqlc.arg(executor)::text, 'source_workbook', sqlc.arg(payload)::json, sqlc.arg(workspace_id)::text, 1, 'pending',
        (now() AT TIME ZONE 'UTC'), (now() AT TIME ZONE 'UTC'), 3, 0)
RETURNING id;
