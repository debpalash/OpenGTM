-- Automation emitters beyond row_added (events.py on_rows_changed, emit_audience_membership).
-- `triggers` is a tenant table (FORCE RLS): call only inside tenancy.InWorkspace. `jobs` is global.

-- name: ListEnabledTriggersWithConfig :many
SELECT id, trigger_config, scope_workbook_ids
FROM triggers
WHERE workspace_id = sqlc.arg(workspace_id)::text
  AND trigger_type = sqlc.arg(trigger_type)::text
  AND enabled IS TRUE
ORDER BY id;

-- name: InsertTriggerEvalJobFor :exec
-- queue_service.add_job(priority=1, executor=go_executor.trigger_eval_executor(...)); no fire_key column.
INSERT INTO jobs (executor, type, payload, workspace_id, priority, status,
                  created_at, next_run_at, max_retries, retry_count)
VALUES (sqlc.arg(executor)::text, 'trigger_eval', sqlc.arg(payload)::json, sqlc.arg(workspace_id)::text, 1, 'pending',
        (now() AT TIME ZONE 'UTC'), (now() AT TIME ZONE 'UTC'), 3, 0);

-- name: AudienceRowTargets :many
-- Workbook rows linked to a lead in the workspace (optionally limited to a scope), capped.
SELECT r.id::int4 AS row_id, r.workbook_id
FROM record_workbook_rows r
JOIN workbooks w ON w.id = r.workbook_id
WHERE w.workspace_id = sqlc.arg(workspace_id)::text
  AND r.lead_id = sqlc.arg(lead_id)::int4
  AND (cardinality(sqlc.arg(scope)::text[]) = 0 OR r.workbook_id = ANY(sqlc.arg(scope)::text[]))
ORDER BY r.id
LIMIT sqlc.arg(max_rows)::int4;
