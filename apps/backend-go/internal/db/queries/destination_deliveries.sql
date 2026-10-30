-- plan 10 (destinations): destination_deliveries. RLS table; run inside
-- tenancy.InWorkspace with the explicit workspace filter as well. Timestamps are naive
-- UTC columns; updated_at = now() mirrors SQLAlchemy's onupdate=func.now().

-- name: DestDeliveryCounts :one
SELECT coalesce(sum(CASE WHEN attempts > 0 THEN 1 ELSE 0 END), 0)::bigint AS attempted,
       coalesce(sum(CASE WHEN status = 'success' THEN 1 ELSE 0 END), 0)::bigint AS succeeded,
       coalesce(sum(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0)::bigint AS failed,
       coalesce(sum(CASE WHEN status = 'skipped' THEN 1 ELSE 0 END), 0)::bigint AS skipped
FROM destination_deliveries
WHERE workspace_id = sqlc.arg(workspace_id)::text AND run_id = sqlc.arg(run_id)::text;

-- name: DestDeliveriesCancelOpen :exec
UPDATE destination_deliveries SET status = 'cancelled', error = 'Run cancelled before delivery', updated_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND run_id = sqlc.arg(run_id)::text
  AND status IN ('pending', 'in_flight');

-- name: DestDeliveriesResetInFlight :exec
UPDATE destination_deliveries SET status = 'pending', error = sqlc.arg(error)::text, updated_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND run_id = sqlc.arg(run_id)::text
  AND status = 'in_flight';

-- name: DestDeliveriesFailOpen :exec
UPDATE destination_deliveries SET status = 'failed', error = sqlc.arg(error)::text, updated_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND run_id = sqlc.arg(run_id)::text
  AND status IN ('pending', 'in_flight');

-- name: DestDeliveriesList :many
SELECT * FROM destination_deliveries
WHERE workspace_id = sqlc.arg(workspace_id)::text AND run_id = sqlc.arg(run_id)::text
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
  AND (sqlc.narg(after_id)::int IS NULL OR id > sqlc.narg(after_id)::int)
ORDER BY id
LIMIT sqlc.arg(row_limit)::int;

-- name: DestDeliveryTotals :one
SELECT count(id)::bigint AS total,
       coalesce(sum(CASE WHEN status = 'success' THEN 1 ELSE 0 END), 0)::bigint AS succeeded,
       coalesce(sum(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0)::bigint AS failed
FROM destination_deliveries
WHERE workspace_id = sqlc.arg(workspace_id)::text AND destination_id = sqlc.arg(destination_id)::text;

-- name: DestRecentFailures :many
SELECT * FROM destination_deliveries
WHERE workspace_id = sqlc.arg(workspace_id)::text AND destination_id = sqlc.arg(destination_id)::text
  AND status = 'failed'
ORDER BY updated_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: DestDeliveryByKey :one
SELECT * FROM destination_deliveries
WHERE workspace_id = sqlc.arg(workspace_id)::text AND idempotency_key = sqlc.arg(idempotency_key)::text;

-- Insert a ledger row, or revive a non-success one in place (Python: a retried
-- idempotent effect moves to the current run, error cleared, payload replaced,
-- attempts counted when the caller delivers immediately). A success row is left
-- untouched (no row returned; the caller skips it).
-- name: DestDeliveryUpsert :one
INSERT INTO destination_deliveries (workspace_id, run_id, destination_id, lead_id, company_entity_id,person_entity_id,subject_key, operation, idempotency_key,
                                    payload_fingerprint, payload, status, attempts, summary)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(run_id)::text, sqlc.arg(destination_id)::text, sqlc.narg(lead_id)::int,sqlc.narg(company_id)::text,sqlc.narg(person_id)::text,sqlc.narg(subject_key)::text,
        sqlc.arg(operation)::text, sqlc.arg(idempotency_key)::text, sqlc.arg(payload_fingerprint)::text,
        sqlc.arg(payload)::json, sqlc.arg(status)::text, sqlc.arg(attempts)::int, sqlc.narg(summary)::text)
ON CONFLICT (workspace_id, idempotency_key) DO UPDATE SET
  run_id = EXCLUDED.run_id, status = EXCLUDED.status, error = NULL, payload = EXCLUDED.payload,
  attempts = destination_deliveries.attempts + EXCLUDED.attempts,
  summary = coalesce(EXCLUDED.summary, destination_deliveries.summary), updated_at = (now() AT TIME ZONE 'UTC')
WHERE destination_deliveries.status <> 'success'
RETURNING *, (xmax = 0) AS inserted;

-- name: DestDeliverySettle :one
UPDATE destination_deliveries SET status = sqlc.arg(status)::text, summary = sqlc.narg(summary)::text,
  error = sqlc.narg(error)::text, external_id = sqlc.narg(external_id)::text,
  delivered_at = CASE WHEN sqlc.arg(delivered)::boolean THEN (now() AT TIME ZONE 'UTC') ELSE delivered_at END,
  updated_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::int
RETURNING *;
