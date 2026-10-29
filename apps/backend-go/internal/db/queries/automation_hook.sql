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
