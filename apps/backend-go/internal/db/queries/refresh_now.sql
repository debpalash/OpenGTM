-- name: EnqueueRefreshNowJob :one
-- Manual refresh (queue_service.add_job without a fire key): the executor is the
-- workspace's cohort resolution. `jobs` is a global control table; naive UTC timestamps.
INSERT INTO jobs (executor, type, payload, workspace_id, priority, status,
                  created_at, next_run_at, max_retries, retry_count)
VALUES (sqlc.arg(executor)::text, 'refresh_workbook', sqlc.arg(payload)::json, sqlc.arg(workspace_id)::text, 1, 'pending',
        (now() AT TIME ZONE 'UTC'), (now() AT TIME ZONE 'UTC'), 3, 0)
RETURNING id;
