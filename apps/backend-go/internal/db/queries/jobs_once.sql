-- Single-flight job insertion (port of job_scheduling.enqueue_job_once). `jobs`
-- is a global control table. Timestamps are naive UTC columns.

-- name: LockJobFireKey :exec
SELECT pg_advisory_xact_lock(hashtext(sqlc.arg(fire_key)::text));

-- name: ActiveJobByFireKey :one
SELECT id FROM jobs
WHERE fire_key = sqlc.arg(fire_key)::text AND status IN ('pending', 'processing')
LIMIT 1;

-- name: InsertJobOnce :one
INSERT INTO jobs (executor, type, payload, workspace_id, priority, fire_key, status,
                  created_at, next_run_at, max_retries, retry_count)
VALUES (sqlc.arg(executor)::text, sqlc.arg(job_type)::text, sqlc.arg(payload)::json,
        NULLIF(sqlc.arg(workspace_id)::text, ''), 1, sqlc.arg(fire_key)::text, 'pending',
        (now() AT TIME ZONE 'UTC'), (now() AT TIME ZONE 'UTC'), 3, 0)
ON CONFLICT (fire_key) WHERE status IN ('pending', 'processing') DO NOTHING
RETURNING id;
