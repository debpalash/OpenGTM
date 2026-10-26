-- Automation row-added hook (port of automations/events.py). `triggers` is a
-- tenant table (FORCE RLS): call only inside tenancy.InWorkspace. `jobs` is a
-- global control table. Timestamps are naive UTC columns.

-- name: ListEnabledTriggersByType :many
SELECT id, scope_workbook_ids
FROM triggers
WHERE workspace_id = sqlc.arg(workspace_id)::text
  AND trigger_type = sqlc.arg(trigger_type)::text
  AND enabled IS TRUE
ORDER BY id;

-- name: InsertTriggerEvalJob :exec
INSERT INTO jobs (executor, type, payload, workspace_id, priority, status,
                  created_at, next_run_at, max_retries, retry_count)
VALUES ('python', 'trigger_eval', sqlc.arg(payload)::json, sqlc.arg(workspace_id)::text, 1, 'pending',
        (now() AT TIME ZONE 'UTC'), (now() AT TIME ZONE 'UTC'), 3, 0);
