-- plan 08 (automations) task 14: trigger_eval handler (internal/autoengine).
-- Every statement runs inside tenancy.InWorkspace (the tables are FORCE RLS; the
-- workspace_id predicate is belt and braces).

-- name: AutoRunTrigger :one
SELECT * FROM triggers WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AutoRunInsertRun :exec
-- The run row of an eval; committed before any debit. finished_at is set only
-- for the `skipped` run of a missing or paused rule.
INSERT INTO trigger_runs (id, workspace_id, trigger_id, trigger_name_snapshot, job_id, status, fire_source, fire_key,
                          matched_rows, actions_attempted, actions_succeeded, actions_skipped, actions_failed,
                          total_charged_usd, finished_at)
VALUES (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(trigger_id)::text,
        sqlc.narg(trigger_name_snapshot)::text, sqlc.arg(job_id)::int4, sqlc.arg(status)::text,
        sqlc.arg(fire_source)::text, sqlc.arg(fire_key)::text, 0, 0, 0, 0, 0, 0,
        sqlc.narg(finished_at)::timestamptz);

-- name: AutoRunFinishRun :execrows
UPDATE trigger_runs
SET matched_rows = sqlc.arg(matched_rows)::int4, actions_attempted = sqlc.arg(actions_attempted)::int4,
    actions_succeeded = sqlc.arg(actions_succeeded)::int4, actions_skipped = sqlc.arg(actions_skipped)::int4,
    actions_failed = sqlc.arg(actions_failed)::int4, total_charged_usd = sqlc.arg(total_charged_usd)::float8,
    status = sqlc.arg(status)::text, finished_at = now()
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AutoRunSetLastFired :exec
UPDATE triggers SET last_fired_at = now()
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AutoRunWorkbooksByIDs :many
-- Workbooks of the workspace; an empty id list means every workbook (the
-- unscoped sweep). Ordered so a sweep is deterministic (Python had no order).
SELECT id, columns_config FROM workbooks
WHERE workspace_id = sqlc.arg(workspace_id)::text
  AND (cardinality(sqlc.arg(ids)::text[]) = 0 OR id = ANY(sqlc.arg(ids)::text[]))
ORDER BY created_at ASC, id ASC;

-- name: AutoRunWorkbookColumns :one
SELECT columns_config FROM workbooks
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AutoRunRowByID :one
SELECT id, workbook_id, lead_id, data, enrichments FROM record_workbook_rows
WHERE id = sqlc.arg(id)::int4 AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AutoRunSweepRows :many
SELECT id, workbook_id, lead_id, data, enrichments FROM record_workbook_rows
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg(row_limit)::int4;

-- name: AutoRunResultByIdem :one
SELECT id, status, action_type, COALESCE(charged_usd, 0)::float8 AS charged_usd FROM trigger_action_results
WHERE workspace_id = sqlc.arg(workspace_id)::text AND idempotency_key = sqlc.arg(idempotency_key)::text;

-- name: AutoRunInsertResult :one
-- One row per idempotency key (uq_trigger_action_idem): a concurrent insert of
-- the same key returns no row and the caller re-reads the winner.
INSERT INTO trigger_action_results (workspace_id, run_id, trigger_id, workbook_id, row_id, lead_id, action_index,
                                    action_type, idempotency_key, status, skip_reason, result_summary, error, charged_usd)
VALUES (sqlc.arg(workspace_id)::text, sqlc.arg(run_id)::text, sqlc.arg(trigger_id)::text, sqlc.arg(workbook_id)::text,
        sqlc.arg(row_id)::text, sqlc.narg(lead_id)::int4, sqlc.arg(action_index)::int4, sqlc.arg(action_type)::text,
        sqlc.arg(idempotency_key)::text, sqlc.arg(status)::text, sqlc.narg(skip_reason)::text,
        sqlc.narg(result_summary)::text, sqlc.narg(error)::text, sqlc.arg(charged_usd)::float8)
ON CONFLICT (workspace_id, idempotency_key) DO NOTHING
RETURNING id;

-- name: AutoRunMarkReplay :execrows
-- A leftover in_flight marker of a non-idempotent action: at most once, never re-sent.
UPDATE trigger_action_results SET status = 'skipped', skip_reason = 'replay', updated_at = now()
WHERE id = sqlc.arg(id)::int4 AND workspace_id = sqlc.arg(workspace_id)::text AND status = 'in_flight';

-- name: AutoRunReuseMarker :exec
-- An idempotent action re-attempts on its existing in_flight marker.
UPDATE trigger_action_results SET charged_usd = sqlc.arg(charged_usd)::float8, updated_at = now()
WHERE id = sqlc.arg(id)::int4 AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AutoRunSettleResult :exec
UPDATE trigger_action_results
SET status = sqlc.arg(status)::text, skip_reason = sqlc.narg(skip_reason)::text,
    result_summary = sqlc.narg(result_summary)::text, error = sqlc.narg(error)::text,
    charged_usd = sqlc.arg(charged_usd)::float8, updated_at = now()
WHERE id = sqlc.arg(id)::int4 AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: AutoRunInsertActivity :exec
INSERT INTO workbook_activity (workbook_id, workspace_id, kind, message)
VALUES (sqlc.arg(workbook_id)::text, sqlc.arg(workspace_id)::text, 'automation', sqlc.arg(message)::text);
