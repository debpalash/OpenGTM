-- Workbook spend ledger. All tables here are tenant tables under FORCE RLS:
-- callers run these inside tenancy.InWorkspace. Sums are cast to text so Go can
-- add them without int64 overflow (Python integers are unbounded).

-- name: LockWorkbook :execrows
UPDATE workbooks SET budget_spent_usd = budget_spent_usd
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: GetAttemptByKey :one
SELECT id, status, contract_hash FROM workbook_spend_attempts
WHERE workspace_id = sqlc.arg(workspace_id)::text AND attempt_key = sqlc.arg(attempt_key)::text;

-- name: GetWorkbookBudget :one
SELECT budget_max_usd, budget_spent_usd FROM workbooks
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: SumOutstandingMicroUSD :one
SELECT COALESCE(SUM(reserved_microusd), 0)::text AS total FROM workbook_spend_attempts
WHERE workspace_id = sqlc.arg(workspace_id)::text AND workbook_id = sqlc.arg(workbook_id)::text
  AND status IN ('reserved', 'dispatched', 'uncertain');

-- name: SumCellExposureMicroUSD :one
SELECT COALESCE(SUM(COALESCE(settled_microusd, reserved_microusd)), 0)::text AS total
FROM workbook_spend_attempts
WHERE workspace_id = sqlc.arg(workspace_id)::text AND workbook_id = sqlc.arg(workbook_id)::text
  AND run_id = sqlc.arg(run_id)::text AND row_identity = sqlc.arg(row_identity)::text
  AND column_id = sqlc.arg(column_id)::text AND status <> 'released';

-- name: InsertAttempt :exec
INSERT INTO workbook_spend_attempts
 (id, workspace_id, workbook_id, run_id, row_identity, column_id, provider, attempt_key,
  contract_hash, reserved_microusd, cost_basis, status, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(workspace_id), sqlc.arg(workbook_id), sqlc.arg(run_id),
  sqlc.arg(row_identity), sqlc.arg(column_id), sqlc.arg(provider), sqlc.arg(attempt_key),
  sqlc.arg(contract_hash), sqlc.arg(reserved_microusd), sqlc.arg(cost_basis), 'reserved',
  sqlc.arg(now_epoch)::float8, sqlc.arg(now_epoch)::float8);

-- name: LockAttemptForSettle :one
SELECT status, settled_microusd, result FROM workbook_spend_attempts
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
  AND workbook_id = sqlc.arg(workbook_id)::text AND contract_hash = sqlc.arg(contract_hash)::text
FOR UPDATE;

-- name: MarkSettled :exec
UPDATE workbook_spend_attempts
SET status = 'settled', settled_microusd = sqlc.arg(settled_microusd), result = sqlc.arg(result),
    updated_at = sqlc.arg(now_epoch)::float8
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AddWorkbookSpend :exec
UPDATE workbooks SET budget_spent_usd = COALESCE(budget_spent_usd, 0) + sqlc.arg(delta_usd)::float8
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: TransitionAttempt :execrows
UPDATE workbook_spend_attempts SET status = sqlc.arg(next_status)::text, updated_at = sqlc.arg(now_epoch)::float8
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
  AND workbook_id = sqlc.arg(workbook_id)::text AND contract_hash = sqlc.arg(contract_hash)::text
  AND status = sqlc.arg(prev_status)::text
  AND (sqlc.narg(run_id)::text IS NULL OR run_id = sqlc.narg(run_id)::text);

-- name: LockOwnedRunWorkbookJob :one
-- Port of batch_attempts._owned_job: same lock strength as its no-op UPDATE.
SELECT payload FROM jobs
WHERE id = sqlc.arg(id)::int AND type = 'run_workbook' AND workspace_id = sqlc.arg(workspace_id)::text
  AND worker_id = sqlc.arg(worker_id)::text AND status = 'processing'
  AND locked_at = sqlc.arg(locked_at) AND lease_token = sqlc.arg(lease_token)::bigint
FOR NO KEY UPDATE;
