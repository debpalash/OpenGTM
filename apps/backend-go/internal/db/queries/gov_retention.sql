-- Retention policy store, preview counts and scheduling (internal/retention).
-- retention_policies and the eight target tables are FORCE RLS: run inside
-- tenancy.InWorkspace, with the explicit workspace filter as well. retention_schedules and jobs
-- are non-RLS control tables. Naive timestamp columns are compared and written as
-- timestamptz so Postgres reads them in the session TimeZone exactly as
-- psycopg/SQLAlchemy do for Python's aware datetimes (see govaudit/gov_audit.sql).

-- name: RetCountAudit :one
SELECT count(*)::bigint FROM governance_audit_events
WHERE workspace_id = sqlc.arg(workspace_id)::text AND created_at < sqlc.arg(cutoff)::timestamptz;

-- llm_usage_daily.date is a 'YYYY-MM-DD' string: string comparison, as in Python.
-- name: RetCountLLMUsage :one
SELECT count(*)::bigint FROM llm_usage_daily
WHERE workspace_id = sqlc.arg(workspace_id)::text AND date < sqlc.arg(cutoff)::text;

-- signals.created_at is a float epoch.
-- name: RetCountSignals :one
SELECT count(*)::bigint FROM signals
WHERE workspace_id = sqlc.arg(workspace_id)::text AND created_at < sqlc.arg(cutoff)::float8;

-- name: RetCountDeliveries :one
SELECT count(*)::bigint FROM destination_deliveries
WHERE workspace_id = sqlc.arg(workspace_id)::text AND created_at < sqlc.arg(cutoff)::timestamptz;

-- name: RetCountInboundReceipts :one
SELECT count(*)::bigint FROM destination_inbound_receipts
WHERE workspace_id = sqlc.arg(workspace_id)::text AND created_at < sqlc.arg(cutoff)::timestamptz;

-- name: RetCountMembershipEvents :one
SELECT count(*)::bigint FROM audience_membership_events
WHERE workspace_id = sqlc.arg(workspace_id)::text AND created_at < sqlc.arg(cutoff)::timestamptz;

-- name: RetCountPlaybookResults :one
SELECT count(*)::bigint FROM playbook_results
WHERE workspace_id = sqlc.arg(workspace_id)::text AND created_at < sqlc.arg(cutoff)::timestamptz;

-- outreach_sends.created_at is timestamptz.
-- name: RetCountOutreachSends :one
SELECT count(*)::bigint FROM outreach_sends
WHERE workspace_id = sqlc.arg(workspace_id)::text AND created_at < sqlc.arg(cutoff)::timestamptz;

-- name: RetGetPolicy :one
SELECT workspace_id, enabled, legal_hold, retention_days, next_run_at, updated_by, created_at, updated_at
FROM retention_policies WHERE workspace_id = sqlc.arg(workspace_id)::text;

-- created_at/updated_at default to now() (session TimeZone, naive) on insert; an update
-- sets updated_at = now() like SQLAlchemy's onupdate=func.now(). next_run_at is left
-- alone (SchedulePolicy owns it).
-- name: RetUpsertPolicy :one
INSERT INTO retention_policies (workspace_id, enabled, legal_hold, retention_days, updated_by)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(enabled)::bool, sqlc.arg(legal_hold)::bool,
        sqlc.arg(retention_days)::json, sqlc.narg(updated_by)::int)
ON CONFLICT (workspace_id) DO UPDATE
SET enabled = EXCLUDED.enabled, legal_hold = EXCLUDED.legal_hold,
    retention_days = EXCLUDED.retention_days, updated_by = EXCLUDED.updated_by,
    updated_at = now()
RETURNING workspace_id, enabled, legal_hold, retention_days, next_run_at, updated_by, created_at, updated_at;

-- name: RetSetPolicyNextRun :one
UPDATE retention_policies
SET next_run_at = (sqlc.narg(next_run_at)::timestamptz)::timestamp, updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id)::text
RETURNING next_run_at, updated_at;

-- Exact prefix match: Python's LIKE treats '_' and '%' in a workspace id as wildcards
-- and could cancel another tenant's pending job.
-- name: RetCancelPendingJobs :execrows
UPDATE jobs SET status = 'cancelled'
WHERE type = 'retention_enforce' AND status = 'pending'
  AND starts_with(fire_key, sqlc.arg(fire_key_prefix)::text)
  -- exact shape prefix || 'YYYY-MM-DD': a workspace id containing ':' ('a:b') would
  -- otherwise match the prefix of workspace 'a'.
  AND length(fire_key) = length(sqlc.arg(fire_key_prefix)::text) + 10;

-- name: RetUpsertMirror :exec
INSERT INTO retention_schedules (workspace_id, enabled, next_run_at)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(enabled)::bool, (sqlc.narg(next_run_at)::timestamptz)::timestamp)
ON CONFLICT (workspace_id) DO UPDATE
SET enabled = EXCLUDED.enabled, next_run_at = EXCLUDED.next_run_at, updated_at = now();

-- The row enqueue_job_once writes: priority 1, max_retries 3, next_run_at = the next
-- occurrence (NOT now: the job must wait a day), workspace_id left NULL as in Python.
-- name: RetInsertScheduledJob :one
INSERT INTO jobs (executor, type, payload, priority, fire_key, status, created_at, next_run_at, max_retries, retry_count)
VALUES (sqlc.arg(executor)::text, 'retention_enforce', sqlc.arg(payload)::json, 1, sqlc.arg(fire_key)::text, 'pending',
        (now() AT TIME ZONE 'UTC'), (sqlc.arg(next_run_at)::timestamptz)::timestamp, 3, 0)
ON CONFLICT (fire_key) WHERE status IN ('pending', 'processing') DO NOTHING
RETURNING id;

-- ---- enforcement and failure reconcile (enforce.go, reconcile.go) ----
-- retention_runs is FORCE RLS; jobs is not. Naive timestamps are written from a
-- timestamptz so Postgres stores them in the session TimeZone (what psycopg does for
-- Python's aware datetimes) and read back as timestamptz for the same reason.

-- name: RetRunGet :one
SELECT id, workspace_id, status, requested_by, policy_snapshot, deleted_counts, error,
       (started_at::timestamptz) AS started_at
FROM retention_runs
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- Locks the run row so a concurrent cancel waits for the batch in flight.
-- name: RetRunLock :one
SELECT status, (started_at::timestamptz) AS started_at
FROM retention_runs
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
FOR UPDATE;

-- name: RetPolicyLockShared :one
SELECT workspace_id, enabled, legal_hold, retention_days, next_run_at, updated_by, created_at, updated_at
FROM retention_policies WHERE workspace_id = sqlc.arg(workspace_id)::text
FOR SHARE;

-- name: RetRunCreate :one
INSERT INTO retention_runs (id, workspace_id, status, requested_by, policy_snapshot, deleted_counts)
VALUES (gen_random_uuid()::text, sqlc.arg(workspace_id)::text, 'pending', 'scheduler', sqlc.arg(policy_snapshot)::json, '{}'::json)
RETURNING id;

-- name: RetRunCancel :exec
UPDATE retention_runs
SET status = 'cancelled', error = sqlc.arg(error)::text, finished_at = (sqlc.arg(finished_at)::timestamptz)::timestamp
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: RetRunStart :exec
UPDATE retention_runs
SET status = 'running', started_at = (sqlc.arg(started_at)::timestamptz)::timestamp
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: RetRunFinish :exec
UPDATE retention_runs
SET status = sqlc.arg(status)::text, deleted_counts = sqlc.arg(deleted_counts)::json,
    error = sqlc.narg(error)::text, finished_at = (sqlc.arg(finished_at)::timestamptz)::timestamp
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- Counts so far for a run another actor already cancelled (status and error stay).
-- name: RetRunSetCounts :exec
UPDATE retention_runs SET deleted_counts = sqlc.arg(deleted_counts)::json
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- Reconcile transitions.
-- name: RetRunRetry :exec
UPDATE retention_runs SET status = 'pending', error = sqlc.arg(error)::text, finished_at = NULL
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: RetJobGet :one
SELECT status, payload FROM jobs WHERE id = sqlc.arg(id)::int;

-- name: RetJobSetPayload :exec
UPDATE jobs SET payload = sqlc.arg(payload)::json WHERE id = sqlc.arg(id)::int;

-- name: RetJobCancelPending :exec
UPDATE jobs SET status = 'cancelled', error = sqlc.arg(error)::text, completed_at = (sqlc.arg(completed_at)::timestamptz)::timestamp
WHERE id = sqlc.arg(id)::int AND status = 'pending';

-- Batched deletes: the explicit workspace predicate is in both the inner select and the
-- outer delete (RLS is the second fence, not the only one).
-- name: RetDeleteAudit :execrows
DELETE FROM governance_audit_events
WHERE workspace_id = sqlc.arg(workspace_id)::text AND ctid IN (
  SELECT ctid FROM governance_audit_events
  WHERE workspace_id = sqlc.arg(workspace_id)::text AND created_at < sqlc.arg(cutoff)::timestamptz
  LIMIT sqlc.arg(batch)::int);

-- name: RetDeleteLLMUsage :execrows
DELETE FROM llm_usage_daily
WHERE workspace_id = sqlc.arg(workspace_id)::text AND ctid IN (
  SELECT ctid FROM llm_usage_daily
  WHERE workspace_id = sqlc.arg(workspace_id)::text AND date < sqlc.arg(cutoff)::text
  LIMIT sqlc.arg(batch)::int);

-- name: RetDeleteSignals :execrows
DELETE FROM signals
WHERE workspace_id = sqlc.arg(workspace_id)::text AND ctid IN (
  SELECT ctid FROM signals
  WHERE workspace_id = sqlc.arg(workspace_id)::text AND created_at < sqlc.arg(cutoff)::float8
  LIMIT sqlc.arg(batch)::int);

-- name: RetDeleteDeliveries :execrows
DELETE FROM destination_deliveries
WHERE workspace_id = sqlc.arg(workspace_id)::text AND ctid IN (
  SELECT ctid FROM destination_deliveries
  WHERE workspace_id = sqlc.arg(workspace_id)::text AND created_at < sqlc.arg(cutoff)::timestamptz
  LIMIT sqlc.arg(batch)::int);

-- name: RetDeleteInboundReceipts :execrows
DELETE FROM destination_inbound_receipts
WHERE workspace_id = sqlc.arg(workspace_id)::text AND ctid IN (
  SELECT ctid FROM destination_inbound_receipts
  WHERE workspace_id = sqlc.arg(workspace_id)::text AND created_at < sqlc.arg(cutoff)::timestamptz
  LIMIT sqlc.arg(batch)::int);

-- name: RetDeleteMembershipEvents :execrows
DELETE FROM audience_membership_events
WHERE workspace_id = sqlc.arg(workspace_id)::text AND ctid IN (
  SELECT ctid FROM audience_membership_events
  WHERE workspace_id = sqlc.arg(workspace_id)::text AND created_at < sqlc.arg(cutoff)::timestamptz
  LIMIT sqlc.arg(batch)::int);

-- name: RetDeletePlaybookResults :execrows
DELETE FROM playbook_results
WHERE workspace_id = sqlc.arg(workspace_id)::text AND ctid IN (
  SELECT ctid FROM playbook_results
  WHERE workspace_id = sqlc.arg(workspace_id)::text AND created_at < sqlc.arg(cutoff)::timestamptz
  LIMIT sqlc.arg(batch)::int);

-- name: RetDeleteOutreachSends :execrows
DELETE FROM outreach_sends
WHERE workspace_id = sqlc.arg(workspace_id)::text AND ctid IN (
  SELECT ctid FROM outreach_sends
  WHERE workspace_id = sqlc.arg(workspace_id)::text AND created_at < sqlc.arg(cutoff)::timestamptz
  LIMIT sqlc.arg(batch)::int);
