-- Workbook run queries for the Go run_workbook executor. Every statement runs
-- inside tenancy.InWorkspace (FORCE RLS). provider_stats is a global table.

-- name: GetWorkbookForRun :one
SELECT id, status, columns_config, source_type, source_config, budget_max_usd, budget_spent_usd
FROM workbooks
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: GetWorkbookStatus :one
SELECT status FROM workbooks
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: CountWorkbookRows :one
SELECT COUNT(*)::bigint FROM workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: ListWorkbookRows :many
SELECT id, lead_id, data, enrichments FROM workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
  AND (NOT sqlc.arg(by_row_ids)::bool OR id = ANY(sqlc.arg(row_ids)::int[]))
  AND (NOT sqlc.arg(by_lead_ids)::bool OR lead_id = ANY(sqlc.arg(lead_ids)::int[]))
ORDER BY id;

-- name: ListRowEnrichments :many
SELECT id, enrichments FROM workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
  AND id = ANY(sqlc.arg(row_ids)::int[]);

-- name: MarkWorkbookRunning :exec
UPDATE workbooks SET status = 'running', updated_at = now()
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text AND status IS DISTINCT FROM 'paused';

-- name: MarkWorkbookComplete :exec
UPDATE workbooks SET status = 'complete', updated_at = now()
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: SetWorkbookProgress :exec
UPDATE workbooks SET total_rows = sqlc.arg(total_rows)::int, completed_rows = sqlc.arg(completed_rows)::int, updated_at = now()
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: FinalizeWorkbook :one
-- Python only writes the final status when the workbook is not paused; the
-- returned status is the effective one (paused wins).
UPDATE workbooks SET
  status = CASE WHEN status IS DISTINCT FROM 'paused' THEN sqlc.arg(final_status)::text ELSE status END,
  completed_rows = CASE WHEN status IS DISTINCT FROM 'paused' THEN sqlc.arg(completed_rows)::int ELSE completed_rows END,
  updated_at = now()
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
RETURNING status;

-- name: UpsertEnrichment :exec
INSERT INTO workbook_enrichments
  (workbook_id, workspace_id, lead_id, column_id, value, status, provider, error, cell_metadata)
VALUES (sqlc.arg(workbook_id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(lead_id)::int, sqlc.arg(column_id)::text,
  sqlc.narg(value)::text, sqlc.arg(status)::text, sqlc.narg(provider)::text, sqlc.narg(error)::text, sqlc.narg(cell_metadata)::json)
ON CONFLICT (workbook_id, lead_id, column_id) DO UPDATE SET
  value = EXCLUDED.value, status = EXCLUDED.status, provider = EXCLUDED.provider,
  error = EXCLUDED.error, cell_metadata = EXCLUDED.cell_metadata, updated_at = now();

-- name: MergeRowOverlay :execrows
-- Mirror of _set_enrichment's WorkbookRow.enrichments[column_id] = cell. The
-- merge is a single statement so concurrent cells of one row cannot lose writes.
UPDATE workbook_rows
SET enrichments = jsonb_set(COALESCE(enrichments::jsonb, '{}'::jsonb), ARRAY[sqlc.arg(column_id)::text], sqlc.arg(cell)::jsonb, true)::json,
    updated_at = now()
WHERE id = sqlc.arg(row_id)::int AND workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: ListPriorAttemptProviders :many
SELECT provider FROM workbook_spend_attempts
WHERE workspace_id = sqlc.arg(workspace_id)::text AND workbook_id = sqlc.arg(workbook_id)::text
  AND run_id = sqlc.arg(run_id)::text AND row_identity = sqlc.arg(row_identity)::text
  AND column_id = sqlc.arg(column_id)::text
ORDER BY created_at, id;

-- name: GetProviderStat :one
SELECT attempts, hits, accuracy_score,
       (cooldown_until IS NOT NULL AND cooldown_until > (now() AT TIME ZONE 'UTC'))::bool AS in_cooldown
FROM provider_stats
WHERE provider = sqlc.arg(provider)::text AND field = sqlc.arg(field)::text;

-- name: GetAnyProviderAccuracy :one
SELECT accuracy_score FROM provider_stats
WHERE provider = sqlc.arg(provider)::text AND accuracy_score IS NOT NULL
LIMIT 1;

-- name: RecordProviderAttempt :exec
-- Port of planner.record_attempt: one atomic additive upsert.
INSERT INTO provider_stats
  (provider, field, attempts, hits, total_confidence, total_latency_ms, total_cost_usd,
   cooldown_until, updated_at, accuracy_samples)
VALUES (sqlc.arg(provider)::text, sqlc.arg(field)::text, 1,
  CASE WHEN sqlc.arg(success)::bool THEN 1 ELSE 0 END,
  CASE WHEN sqlc.arg(success)::bool THEN sqlc.arg(confidence)::float8 ELSE 0.0 END,
  sqlc.arg(latency_ms)::float8,
  CASE WHEN sqlc.arg(success)::bool THEN sqlc.arg(cost_usd)::float8 ELSE 0.0 END,
  CASE WHEN sqlc.arg(cooldown_seconds)::int > 0
       THEN (now() AT TIME ZONE 'UTC') + make_interval(secs => sqlc.arg(cooldown_seconds)::int) END,
  (now() AT TIME ZONE 'UTC'), 0)
ON CONFLICT (provider, field) DO UPDATE SET
  attempts = COALESCE(provider_stats.attempts, 0) + 1,
  hits = COALESCE(provider_stats.hits, 0) + CASE WHEN sqlc.arg(success)::bool THEN 1 ELSE 0 END,
  total_confidence = COALESCE(provider_stats.total_confidence, 0.0) + CASE WHEN sqlc.arg(success)::bool THEN sqlc.arg(confidence)::float8 ELSE 0.0 END,
  total_latency_ms = COALESCE(provider_stats.total_latency_ms, 0.0) + sqlc.arg(latency_ms)::float8,
  total_cost_usd = COALESCE(provider_stats.total_cost_usd, 0.0) + CASE WHEN sqlc.arg(success)::bool THEN sqlc.arg(cost_usd)::float8 ELSE 0.0 END,
  updated_at = (now() AT TIME ZONE 'UTC'),
  cooldown_until = CASE WHEN sqlc.arg(cooldown_seconds)::int > 0
       THEN (now() AT TIME ZONE 'UTC') + make_interval(secs => sqlc.arg(cooldown_seconds)::int)
       ELSE provider_stats.cooldown_until END;

-- name: ChargeWorkbookBudget :exec
UPDATE workbooks SET budget_spent_usd = COALESCE(budget_spent_usd, 0) + sqlc.arg(delta_usd)::float8
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: LockLeaseForReceipt :one
-- Port of persist_run_result's predicates (processing OR cancelled) with the
-- row locked so the payload merge cannot interleave with an output claim.
SELECT payload, retry_count FROM jobs
WHERE id = sqlc.arg(id)::int AND type = 'run_workbook' AND workspace_id = sqlc.arg(workspace_id)::text
  AND worker_id = sqlc.arg(worker_id)::text AND lease_token = sqlc.arg(lease_token)::bigint
  AND locked_at = sqlc.arg(locked_at) AND status IN ('processing', 'cancelled')
FOR NO KEY UPDATE;

-- name: StoreReceiptPayload :execrows
UPDATE jobs SET payload = sqlc.arg(payload)::json
WHERE id = sqlc.arg(id)::int AND type = 'run_workbook' AND workspace_id = sqlc.arg(workspace_id)::text
  AND worker_id = sqlc.arg(worker_id)::text AND lease_token = sqlc.arg(lease_token)::bigint
  AND locked_at = sqlc.arg(locked_at) AND status IN ('processing', 'cancelled');
