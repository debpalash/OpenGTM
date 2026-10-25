-- Queue queries for the Go executor. `jobs` is a global control table (no RLS).
-- Timestamps are naive UTC columns; "now" is computed in SQL as UTC.

-- name: SelectClaimCandidate :one
SELECT j.id, j.workspace_id
FROM jobs j
WHERE j.status = 'pending' AND j.executor = 'go'
  AND (j.next_run_at IS NULL OR j.next_run_at <= (now() AT TIME ZONE 'UTC'))
  AND (j.workspace_id IS NULL OR sqlc.arg(tenant_cap)::int = 0 OR
       (SELECT COUNT(*) FROM jobs active WHERE active.status = 'processing'
          AND active.workspace_id = j.workspace_id) < sqlc.arg(tenant_cap)::int)
ORDER BY j.priority DESC,
         (SELECT COUNT(*) FROM jobs active WHERE active.status = 'processing'
            AND active.workspace_id = j.workspace_id) ASC,
         j.next_run_at ASC, j.created_at ASC
FOR UPDATE OF j SKIP LOCKED
LIMIT 1;

-- name: LockTenant :exec
SELECT pg_advisory_xact_lock(hashtext(sqlc.arg(workspace_id)::text));

-- name: CountActiveForWorkspace :one
SELECT COUNT(*)::bigint FROM jobs WHERE status = 'processing' AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: MarkClaimed :one
UPDATE jobs SET status = 'processing', started_at = (now() AT TIME ZONE 'UTC'),
    last_heartbeat = (now() AT TIME ZONE 'UTC'), locked_at = (now() AT TIME ZONE 'UTC'),
    worker_id = sqlc.arg(worker_id)::text, lease_token = lease_token + 1
WHERE id = sqlc.arg(id)
RETURNING id, type, workspace_id, payload, worker_id, locked_at, lease_token;

-- name: FailUnsupported :execrows
UPDATE jobs SET status = 'failed', completed_at = (now() AT TIME ZONE 'UTC'),
    error = 'go executor: unsupported job type', worker_id = NULL, locked_at = NULL
WHERE id = sqlc.arg(id) AND worker_id = sqlc.arg(worker_id)::text
  AND locked_at = sqlc.arg(locked_at) AND lease_token = sqlc.arg(lease_token)
  AND status = 'processing';

-- name: HeartbeatLease :execrows
UPDATE jobs SET last_heartbeat = (now() AT TIME ZONE 'UTC')
WHERE id = sqlc.arg(id) AND worker_id = sqlc.arg(worker_id)::text
  AND locked_at = sqlc.arg(locked_at) AND lease_token = sqlc.arg(lease_token)
  AND status = 'processing';

-- name: LeaseIsProcessing :one
SELECT EXISTS (
  SELECT 1 FROM jobs
  WHERE id = sqlc.arg(id) AND worker_id = sqlc.arg(worker_id)::text
    AND locked_at = sqlc.arg(locked_at) AND lease_token = sqlc.arg(lease_token)
    AND status = 'processing');

-- name: LockLeaseForFinalize :one
SELECT status, retry_count, max_retries, completed_at
FROM jobs
WHERE id = sqlc.arg(id) AND worker_id = sqlc.arg(worker_id)::text
  AND locked_at = sqlc.arg(locked_at) AND lease_token = sqlc.arg(lease_token)
  AND status IN ('processing', 'cancelled')
FOR UPDATE;

-- name: FinalizeCancelled :execrows
UPDATE jobs SET worker_id = NULL, locked_at = NULL,
    completed_at = COALESCE(completed_at, (now() AT TIME ZONE 'UTC'))
WHERE id = sqlc.arg(id) AND worker_id = sqlc.arg(worker_id)::text
  AND locked_at = sqlc.arg(locked_at) AND lease_token = sqlc.arg(lease_token)
  AND status = 'cancelled';

-- name: FinalizeCompleted :execrows
UPDATE jobs SET worker_id = NULL, locked_at = NULL, status = 'completed',
    completed_at = (now() AT TIME ZONE 'UTC'), error = NULL
WHERE id = sqlc.arg(id) AND worker_id = sqlc.arg(worker_id)::text
  AND locked_at = sqlc.arg(locked_at) AND lease_token = sqlc.arg(lease_token)
  AND status = 'processing';

-- name: FinalizeRetry :execrows
UPDATE jobs SET worker_id = NULL, locked_at = NULL, status = 'pending',
    retry_count = sqlc.arg(new_retry_count)::int,
    next_run_at = (now() AT TIME ZONE 'UTC') + make_interval(mins => sqlc.arg(backoff_minutes)::int),
    error = sqlc.arg(error)::text
WHERE id = sqlc.arg(id) AND worker_id = sqlc.arg(worker_id)::text
  AND locked_at = sqlc.arg(locked_at) AND lease_token = sqlc.arg(lease_token)
  AND status = 'processing';

-- name: FinalizeFailed :execrows
UPDATE jobs SET worker_id = NULL, locked_at = NULL, status = 'failed',
    completed_at = (now() AT TIME ZONE 'UTC'), error = sqlc.arg(error)::text
WHERE id = sqlc.arg(id) AND worker_id = sqlc.arg(worker_id)::text
  AND locked_at = sqlc.arg(locked_at) AND lease_token = sqlc.arg(lease_token)
  AND status = 'processing';

-- name: LeaseIsCancelled :one
SELECT EXISTS (
  SELECT 1 FROM jobs
  WHERE id = sqlc.arg(id) AND worker_id = sqlc.arg(worker_id)::text
    AND locked_at = sqlc.arg(locked_at) AND lease_token = sqlc.arg(lease_token)
    AND status = 'cancelled');
