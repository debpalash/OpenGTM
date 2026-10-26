-- plan 14 (leadgen collection): the collection job/stage ledger (port of
-- PgJobLedger, job_ledger.py). collection_jobs and collection_job_stages are
-- FORCE RLS: run inside tenancy.InWorkspace. Every statement also filters
-- workspace_id from the transaction-local GUC (belt). Rows keep SQLite value
-- shapes (ISO TEXT timestamps, '' defaults, JSON TEXT); columns are selected
-- explicitly so JSON key order matches SQLite's SELECT * (workspace_id last).

-- name: CJCreate :execrows
INSERT INTO collection_jobs (id, workspace_id, query, intent, intent_details, created_at)
VALUES (sqlc.arg(id)::text, current_setting('app.workspace_id', true), sqlc.arg(query)::text,
        sqlc.arg(intent)::text, sqlc.arg(intent_details)::text, sqlc.arg(created_at)::text)
ON CONFLICT (id) DO NOTHING;

-- name: CJSetWorkspace :execrows
UPDATE collection_jobs SET workspace_id = sqlc.arg(to_workspace_id)::text
WHERE id = sqlc.arg(id)::text AND workspace_id = current_setting('app.workspace_id', true);

-- name: CJList :many
SELECT id, query, intent, intent_details, status, tier, attempts, max_attempts, leads_found, proxy_used, error, created_at, started_at, completed_at, workspace_id
FROM collection_jobs
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
ORDER BY created_at DESC
LIMIT sqlc.arg(page_limit)::bigint;

-- name: CJGet :one
SELECT id, query, intent, intent_details, status, tier, attempts, max_attempts, leads_found, proxy_used, error, created_at, started_at, completed_at, workspace_id
FROM collection_jobs
WHERE id = sqlc.arg(id)::text AND workspace_id = current_setting('app.workspace_id', true);

-- name: CJStages :many
SELECT id, job_id, stage, status, input_count, output_count, rejected_count, details, started_at, completed_at
FROM collection_job_stages
WHERE job_id = sqlc.arg(job_id)::text AND workspace_id = current_setting('app.workspace_id', true)
ORDER BY id;

-- name: CJCancel :one
UPDATE collection_jobs
SET status = 'cancelled', error = 'Cancelled by user', completed_at = sqlc.arg(completed_at)::text
WHERE id = sqlc.arg(id)::text AND workspace_id = current_setting('app.workspace_id', true)
  AND status IN ('running', 'pending')
RETURNING id;

-- name: CJRetry :one
UPDATE collection_jobs
SET status = 'pending', error = '', completed_at = '', attempts = 0
WHERE id = sqlc.arg(id)::text AND workspace_id = current_setting('app.workspace_id', true)
  AND status IN ('failed', 'cancelled')
RETURNING id;

-- name: CJDelete :execrows
-- Stages go with the job (ON DELETE CASCADE).
DELETE FROM collection_jobs
WHERE id = sqlc.arg(id)::text AND workspace_id = current_setting('app.workspace_id', true);

-- name: CJMarkFailedEnqueue :execrows
UPDATE collection_jobs
SET status = 'failed', error = sqlc.arg(error)::text, completed_at = sqlc.arg(completed_at)::text
WHERE id = sqlc.arg(id)::text AND workspace_id = current_setting('app.workspace_id', true);
