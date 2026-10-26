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
