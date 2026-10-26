-- Python-executor job insertion (port of queue_service.add_job). `jobs` is a
-- global control table. Timestamps are naive UTC columns.

-- name: EnqueuePythonJob :one
INSERT INTO jobs (executor, type, payload, workspace_id, priority, fire_key, status,
                  created_at, next_run_at, max_retries, retry_count)
VALUES ('python', sqlc.arg(job_type)::text, sqlc.arg(payload)::json, NULLIF(sqlc.arg(workspace_id)::text, ''), 1,
        NULLIF(sqlc.arg(fire_key)::text, ''), 'pending',
        (now() AT TIME ZONE 'UTC'), (now() AT TIME ZONE 'UTC'), 3, 0)
RETURNING id;
