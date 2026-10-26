-- MCP write/enrich cap reservations (plan 13, task 6) on trigger_cap_reservations
-- (FORCE RLS: run inside tenancy.InWorkspace). The (workspace_id,
-- idempotency_key) unique constraint is the idempotency ledger.

-- name: McpCapLock :exec
SELECT pg_advisory_xact_lock(hashtextextended('mcpcap:' || sqlc.arg(workspace_id)::text || ':' || sqlc.arg(day_utc)::text, 0));

-- name: McpCapByKey :one
SELECT id, state FROM trigger_cap_reservations
WHERE workspace_id = sqlc.arg(workspace_id)::text AND idempotency_key = sqlc.arg(idempotency_key)::text;

-- name: McpCapActionsToday :one
SELECT COALESCE(SUM(reserved_actions), 0)::bigint FROM trigger_cap_reservations
WHERE workspace_id = sqlc.arg(workspace_id)::text AND trigger_id = sqlc.arg(trigger_id)::text
  AND day_utc = sqlc.arg(day_utc)::text AND state IN ('held', 'settled');

-- name: McpCapInsert :one
INSERT INTO trigger_cap_reservations
    (workspace_id, trigger_id, day_utc, reserved_usd, reserved_actions, idempotency_key, state)
VALUES
    (sqlc.arg(workspace_id)::text, sqlc.arg(trigger_id)::text, sqlc.arg(day_utc)::text, 0, 1,
     sqlc.arg(idempotency_key)::text, 'held')
RETURNING id;

-- name: McpCapReopen :execrows
UPDATE trigger_cap_reservations
SET state = 'held', trigger_id = sqlc.arg(trigger_id)::text, day_utc = sqlc.arg(day_utc)::text, created_at = now()
WHERE id = sqlc.arg(id)::int AND state = 'released';

-- name: McpCapSettle :execrows
UPDATE trigger_cap_reservations SET state = 'settled' WHERE id = sqlc.arg(id)::int AND state = 'held';

-- name: McpCapRelease :execrows
UPDATE trigger_cap_reservations SET state = 'released' WHERE id = sqlc.arg(id)::int AND state = 'held';
