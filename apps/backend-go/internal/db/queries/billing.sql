-- Credit ledger (WI-9). workspace_credits and credit_ledger_entries are tenant
-- tables under FORCE RLS: callers run these inside tenancy.InWorkspace.

-- name: BillingEnsureCredits :exec
INSERT INTO workspace_credits (workspace_id, balance_usd)
VALUES (sqlc.arg(workspace_id)::text, 0.0)
ON CONFLICT (workspace_id) DO NOTHING;

-- name: BillingLockBalance :one
SELECT balance_usd FROM workspace_credits
WHERE workspace_id = sqlc.arg(workspace_id)::text
FOR UPDATE;

-- name: BillingGetBalance :one
SELECT balance_usd FROM workspace_credits
WHERE workspace_id = sqlc.arg(workspace_id)::text;

-- name: BillingLedgerKeyExists :one
SELECT EXISTS (
  SELECT 1 FROM credit_ledger_entries WHERE idempotency_key = sqlc.arg(idempotency_key)::text
) AS found;

-- name: BillingInsertEntry :exec
INSERT INTO credit_ledger_entries (workspace_id, amount_usd, reason, run_id, idempotency_key)
VALUES (
  sqlc.arg(workspace_id)::text, sqlc.arg(amount_usd)::float8, sqlc.arg(reason)::text,
  NULL, sqlc.narg(idempotency_key)::text
);

-- name: BillingSetBalance :exec
UPDATE workspace_credits
SET balance_usd = sqlc.arg(balance_usd)::float8, updated_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text;

-- name: BillingRecentEntries :many
SELECT id, workspace_id, amount_usd, reason, run_id, created_at
FROM credit_ledger_entries
WHERE workspace_id = sqlc.arg(workspace_id)::text
ORDER BY id DESC
LIMIT sqlc.arg(row_limit)::int;
