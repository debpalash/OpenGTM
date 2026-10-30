-- Signal scan (port of services/signals/monitor.py run_signal_scan + store.py
-- add_signal + workbook/refresh.py handle_signal_scan). `workspaces` and `jobs`
-- are global tables; signals, leads, triggers, workbooks, workbook_rows and
-- workbook_activity are FORCE RLS: run those only inside tenancy.InWorkspace.
-- Timestamps in jobs are naive UTC columns.

-- name: ScanListWorkspaceIDs :many
SELECT id FROM workspaces ORDER BY created_at ASC, id ASC;

-- name: ScanInsertSignal :one
-- Idempotent on the deterministic id (store.add_signal: insert-only-when-absent;
-- the unique-violation race is the DO NOTHING). No row back = already present.
INSERT INTO signals (id, workspace_id, lead_id, company, signal_type, title, description,
                     source, source_url, weight, created_at, read)
VALUES (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(lead_id)::int4,
        sqlc.arg(company)::text, sqlc.arg(signal_type)::text, sqlc.arg(title)::text,
        sqlc.arg(description)::text, sqlc.arg(source)::text, sqlc.arg(source_url)::text,
        sqlc.arg(weight)::int4, sqlc.arg(created_at)::float8, false)
ON CONFLICT (id) DO NOTHING
RETURNING id;

-- name: ScanLeadScore :one
SELECT score FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true) AND id = sqlc.arg(id)::int4;

-- name: ListEnabledSignalTriggers :many
-- on_signal rules of the workspace with their trigger_config (events._enabled_rules).
SELECT id, trigger_config, scope_workbook_ids
FROM triggers
WHERE workspace_id = sqlc.arg(workspace_id)::text
  AND trigger_type = 'on_signal'
  AND enabled IS TRUE
ORDER BY id;

-- name: SignalRowTargets :many
-- Workbook rows linked to a lead in the workspace, optionally limited to a rule's scope.
SELECT r.id::int4 AS row_id, r.workbook_id
FROM record_workbook_rows r
JOIN workbooks w ON w.id = r.workbook_id
WHERE w.workspace_id = sqlc.arg(workspace_id)::text
  AND r.lead_id = sqlc.arg(lead_id)::int4
  AND (cardinality(sqlc.arg(scope)::text[]) = 0 OR r.workbook_id = ANY(sqlc.arg(scope)::text[]))
ORDER BY r.id;

-- name: ScanListWorkbooks :many
SELECT id, refresh_policy FROM workbooks
WHERE workspace_id = sqlc.arg(workspace_id)::text
ORDER BY id;

-- name: InsertSignalRefreshJob :exec
-- _enqueue_next_now: an immediate refresh_workbook, priority 2, no fire key.
INSERT INTO jobs (executor, type, payload, workspace_id, priority, status,
                  created_at, next_run_at, max_retries, retry_count)
VALUES (sqlc.arg(executor)::text, 'refresh_workbook', sqlc.arg(payload)::json,
        sqlc.arg(workspace_id)::text, 2, 'pending',
        (now() AT TIME ZONE 'UTC'), (now() AT TIME ZONE 'UTC'), 3, 0);

-- name: InsertSignalScanJobAt :one
-- Delayed, fire-keyed signal_scan insert (global job: no workspace).
INSERT INTO jobs (executor, type, payload, workspace_id, priority, fire_key, status,
                  created_at, next_run_at, max_retries, retry_count)
VALUES (sqlc.arg(executor)::text, 'signal_scan', sqlc.arg(payload)::json,
        NULL, 1, sqlc.arg(fire_key)::text, 'pending',
        (now() AT TIME ZONE 'UTC'), sqlc.arg(next_run_at)::timestamp, 3, 0)
ON CONFLICT (fire_key) WHERE status IN ('pending', 'processing') DO NOTHING
RETURNING id;
