-- Platform credit debit (plan 06 task 6). Tenant tables under FORCE RLS: run
-- inside the caller's tenancy.InWorkspace transaction. Reuses BillingLockBalance
-- and BillingGetBalance from billing.sql.

-- name: BillingDebitKeyExists :one
-- Replay check: the workspace-scoped key, or the legacy Python "run:<id>" key
-- (RLS limits both to this workspace's rows).
SELECT EXISTS (
  SELECT 1 FROM credit_ledger_entries
  WHERE idempotency_key = ANY(sqlc.arg(idempotency_keys)::text[])
) AS found;

-- name: BillingInsertDebit :exec
INSERT INTO credit_ledger_entries (workspace_id, amount_usd, reason, run_id, idempotency_key)
VALUES (
  sqlc.arg(workspace_id)::text, sqlc.arg(amount_usd)::float8, sqlc.arg(reason)::text,
  sqlc.arg(run_id)::text, sqlc.arg(idempotency_key)::text
);
