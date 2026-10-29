-- plan 08 (automations, task 5): cap reservations on trigger_cap_reservations.
-- Statements run inside tenancy.InWorkspace (FORCE RLS). The (workspace_id,
-- idempotency_key) unique constraint is the idempotency ledger.

-- name: AutoCapLock :exec
SELECT pg_advisory_xact_lock(hashtext('autocap:' || sqlc.arg(workspace_id)::text || ':' || sqlc.arg(day_utc)::text));

-- name: AutoCapByKey :one
SELECT id, state FROM trigger_cap_reservations
WHERE workspace_id = sqlc.arg(workspace_id)::text AND idempotency_key = sqlc.arg(idempotency_key)::text;

-- name: AutoCapActions :one
SELECT COALESCE(SUM(reserved_actions), 0)::bigint FROM trigger_cap_reservations
WHERE workspace_id = sqlc.arg(workspace_id)::text AND trigger_id = sqlc.arg(trigger_id)::text
  AND day_utc = sqlc.arg(day_utc)::text AND state IN ('held', 'settled');

-- name: AutoCapRuleSpend :one
SELECT COALESCE(SUM(reserved_usd), 0)::float8 FROM trigger_cap_reservations
WHERE workspace_id = sqlc.arg(workspace_id)::text AND trigger_id = sqlc.arg(trigger_id)::text
  AND day_utc = sqlc.arg(day_utc)::text AND state IN ('held', 'settled');

-- name: AutoCapWorkspaceSpend :one
SELECT COALESCE(SUM(reserved_usd), 0)::float8 FROM trigger_cap_reservations
WHERE workspace_id = sqlc.arg(workspace_id)::text
  AND day_utc = sqlc.arg(day_utc)::text AND state IN ('held', 'settled');

-- name: AutoCapInsert :one
INSERT INTO trigger_cap_reservations
    (workspace_id, trigger_id, day_utc, reserved_usd, reserved_actions, idempotency_key, state)
VALUES
    (sqlc.arg(workspace_id)::text, sqlc.arg(trigger_id)::text, sqlc.arg(day_utc)::text,
     sqlc.arg(reserved_usd)::float8, 1, sqlc.arg(idempotency_key)::text, 'held')
RETURNING id;

-- name: AutoCapSettle :execrows
UPDATE trigger_cap_reservations SET state = 'settled' WHERE id = sqlc.arg(id)::int AND state = 'held';

-- name: AutoCapRelease :execrows
UPDATE trigger_cap_reservations SET state = 'released' WHERE id = sqlc.arg(id)::int AND state = 'held';
