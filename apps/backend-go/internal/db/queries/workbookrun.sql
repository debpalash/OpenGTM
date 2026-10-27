-- name: EnqueueRunWorkbookJob :one
-- POST /run: ONE run_workbook job with the planned executor ('go' since the plan 15 cutover),
-- inserted in the same transaction as the debit and the status update.
INSERT INTO jobs (executor, type, payload, workspace_id, priority, status,
                  created_at, next_run_at, max_retries, retry_count)
VALUES (sqlc.arg(executor)::text, 'run_workbook', sqlc.arg(payload)::text::json, NULLIF(sqlc.arg(workspace_id)::text, ''), 1, 'pending',
        (now() AT TIME ZONE 'UTC'), (now() AT TIME ZONE 'UTC'), 3, 0)
RETURNING id;
