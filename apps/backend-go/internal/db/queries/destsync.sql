-- plan 10 task 13: destination sync engine statements. Tenant tables (RLS): run inside
-- tenancy.InWorkspace, every statement also filters on the explicit workspace id.
-- Timestamps are naive UTC columns.

-- One keyset page of the audience's members (the sync streams them 500 at a time).
-- name: DSMemberPage :many
SELECT id, lead_id, snapshot FROM audience_members
WHERE workspace_id = sqlc.arg(workspace_id)::text AND audience_id = sqlc.arg(audience_id)::text
  AND id > sqlc.arg(after_id)::int
ORDER BY id
LIMIT sqlc.arg(page_limit)::int;

-- Guarded: a cancel (or reconcile) that landed after the caller's read leaves no row here.
-- name: DSRunStart :one
UPDATE destination_runs
SET status = 'running', started_at = coalesce(started_at, (now() AT TIME ZONE 'UTC')), error = NULL
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text
  AND status IN ('pending', 'running')
RETURNING *;

-- name: DSRunCancelUnusable :exec
UPDATE destination_runs
SET status = 'cancelled', error = 'destination missing or disabled', finished_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text;

-- Terminal completion: counters and status, error left as the start cleared it.
-- name: DSRunComplete :one
UPDATE destination_runs
SET status = sqlc.arg(status)::text, finished_at = (now() AT TIME ZONE 'UTC'),
    attempted = sqlc.arg(attempted)::int, succeeded = sqlc.arg(succeeded)::int,
    failed = sqlc.arg(failed)::int, skipped = sqlc.arg(skipped)::int
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text AND status = 'running'
RETURNING finished_at;

-- name: DSDestSetHealth :exec
UPDATE audience_destinations
SET health_status = sqlc.arg(health_status)::text, last_error = sqlc.narg(last_error)::text,
    last_success_at = CASE WHEN sqlc.arg(set_success)::boolean THEN sqlc.arg(finished_at)::timestamp ELSE last_success_at END,
    updated_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text;

-- name: DSDeliveriesMarkInFlight :exec
UPDATE destination_deliveries
SET status = 'in_flight', attempts = attempts + 1, updated_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = ANY(sqlc.arg(ids)::int[]);

-- Settle a batch together: only rows still in flight (a concurrent reconcile may have
-- closed some) get the shared outcome.
-- name: DSDeliveriesSettle :execrows
UPDATE destination_deliveries
SET status = sqlc.arg(status)::text, summary = sqlc.narg(summary)::text, error = sqlc.narg(error)::text,
    external_id = sqlc.narg(external_id)::text,
    delivered_at = CASE WHEN sqlc.arg(delivered)::boolean THEN (now() AT TIME ZONE 'UTC') ELSE delivered_at END,
    updated_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = ANY(sqlc.arg(ids)::int[]) AND status = 'in_flight';

-- name: DSWarehouseToInFlight :execrows
UPDATE destination_deliveries
SET status = 'in_flight', attempts = attempts + 1, updated_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND run_id = sqlc.arg(run_id)::text
  AND destination_id = sqlc.arg(destination_id)::text AND status = 'pending';

-- name: DSWarehouseSettle :execrows
UPDATE destination_deliveries
SET status = sqlc.arg(status)::text, summary = sqlc.narg(summary)::text, error = sqlc.narg(error)::text,
    external_id = sqlc.narg(external_id)::text,
    delivered_at = CASE WHEN sqlc.arg(delivered)::boolean THEN (now() AT TIME ZONE 'UTC') ELSE delivered_at END,
    updated_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND run_id = sqlc.arg(run_id)::text
  AND destination_id = sqlc.arg(destination_id)::text AND status = 'in_flight';

-- Exit reconciliation: the latest successful delivery per lead, when it is an upsert and
-- the lead has no membership row any more. Keyset by delivery id.
-- name: DSExitCandidates :many
WITH latest AS (
 SELECT DISTINCT ON (coalesce('person:'||person_entity_id,'company:'||company_entity_id,'lead:'||lead_id::text)) * FROM destination_deliveries
 WHERE workspace_id=sqlc.arg(workspace_id)::text AND destination_id=sqlc.arg(destination_id)::text AND status='success'
 ORDER BY coalesce('person:'||person_entity_id,'company:'||company_entity_id,'lead:'||lead_id::text),id DESC
)
SELECT d.id,d.lead_id,d.company_entity_id,d.person_entity_id,d.payload
FROM latest d WHERE d.operation='upsert' AND d.id>sqlc.arg(after_id)::int
AND NOT EXISTS(SELECT 1 FROM audience_members m WHERE m.workspace_id=sqlc.arg(workspace_id)::text AND m.audience_id=sqlc.arg(audience_id)::text AND (m.lead_id=d.lead_id OR (d.person_entity_id IS NOT NULL AND m.person_entity_id=d.person_entity_id) OR (d.person_entity_id IS NULL AND m.person_entity_id IS NULL AND m.company_entity_id=d.company_entity_id)))
ORDER BY d.id LIMIT sqlc.arg(page_limit)::int;

-- A removal row that failed (or was cancelled) earlier is queued again where it is: it
-- keeps its run (as Python does), error cleared.
-- name: DSDeliveryRequeue :exec
UPDATE destination_deliveries SET status = 'pending', error = NULL, updated_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::int;

-- One delivery's result, only while the row is still ours: in flight and in this run (a
-- reconcile may have closed it, or a later run re-homed it, while the request was out).
-- name: DSDeliverySettleOne :execrows
UPDATE destination_deliveries
SET status = sqlc.arg(status)::text, summary = sqlc.narg(summary)::text, error = sqlc.narg(error)::text,
    external_id = sqlc.narg(external_id)::text,
    delivered_at = CASE WHEN sqlc.arg(delivered)::boolean THEN (now() AT TIME ZONE 'UTC') ELSE delivered_at END,
    updated_at = (now() AT TIME ZONE 'UTC')
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::int
  AND run_id = sqlc.arg(run_id)::text AND status = 'in_flight';
