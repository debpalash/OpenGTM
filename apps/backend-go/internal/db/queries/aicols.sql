-- Plan 15 WP-AICOLS: agent column reasoning trace (agent_column._save_trace). Run inside
-- tenancy.InWorkspace; the statements also carry the explicit workspace_id predicate.

-- name: UpdateCellTrace :execrows
-- The first existing trace of the cell is overwritten (Python: query(...).first()).
UPDATE cell_traces SET goal = sqlc.arg(goal)::text, steps = sqlc.arg(steps)::json,
    outcome = sqlc.arg(outcome)::text, total_cost_usd = sqlc.arg(total_cost_usd)::json
WHERE id = (SELECT id FROM cell_traces
            WHERE workspace_id = sqlc.arg(workspace_id)::text AND workbook_id = sqlc.arg(workbook_id)::text
              AND lead_id = sqlc.arg(lead_id)::int AND column_id = sqlc.arg(column_id)::text
            ORDER BY id LIMIT 1);

-- name: InsertCellTrace :exec
INSERT INTO cell_traces (workbook_id, workspace_id, lead_id, column_id, goal, steps, outcome, total_cost_usd)
VALUES (sqlc.arg(workbook_id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(lead_id)::int, sqlc.arg(column_id)::text,
        sqlc.arg(goal)::text, sqlc.arg(steps)::json, sqlc.arg(outcome)::text, sqlc.arg(total_cost_usd)::json);
