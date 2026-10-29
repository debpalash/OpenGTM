-- plan 10 (destinations): destination_runs. RLS table; run inside tenancy.InWorkspace
-- with the explicit workspace filter as well. Timestamps are naive UTC columns.

-- name: DestRunInsert :one
INSERT INTO destination_runs (id, workspace_id, destination_id, requested_by)
VALUES (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(destination_id)::text, sqlc.narg(requested_by)::text)
RETURNING *;

-- name: DestRunGet :one
SELECT * FROM destination_runs
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text;

-- name: DestRunLock :one
SELECT * FROM destination_runs
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text
FOR UPDATE;

-- name: DestRunActive :one
SELECT * FROM destination_runs
WHERE workspace_id = sqlc.arg(workspace_id)::text AND destination_id = sqlc.arg(destination_id)::text
  AND status IN ('pending', 'running', 'cancelling')
ORDER BY created_at, id
LIMIT 1;

-- Single-flight lock per destination (the Python start_sync had none).
-- name: DestLockSync :exec
SELECT pg_advisory_xact_lock(hashtext('destsync:' || sqlc.arg(destination_id)::text));

-- name: DestRunSetPending :one
UPDATE destination_runs SET status = 'pending', error = sqlc.narg(error)::text, finished_at = NULL
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text
RETURNING *;

-- name: DestRunRequestCancel :one
UPDATE destination_runs SET status = 'cancelling', error = 'Cancellation requested'
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text
RETURNING *;

-- name: DestRunSetFinished :one
UPDATE destination_runs SET status = sqlc.arg(status)::text, error = sqlc.arg(error)::text,
  finished_at = (now() AT TIME ZONE 'UTC'),
  attempted = sqlc.arg(attempted)::int, succeeded = sqlc.arg(succeeded)::int,
  failed = sqlc.arg(failed)::int, skipped = sqlc.arg(skipped)::int
WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text
RETURNING *;

-- Keyset page: rows strictly older than the cursor (the cursor instant is aware UTC;
-- Python compares the naive column against it, i.e. as UTC).
-- name: DestRunsList :many
SELECT * FROM destination_runs
WHERE workspace_id = sqlc.arg(workspace_id)::text AND destination_id = sqlc.arg(destination_id)::text
  AND (NOT sqlc.arg(has_cursor)::boolean
       OR created_at < (sqlc.arg(cursor_created_at)::timestamptz AT TIME ZONE 'UTC')
       OR (created_at = (sqlc.arg(cursor_created_at)::timestamptz AT TIME ZONE 'UTC') AND id < sqlc.arg(cursor_id)::text))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int OFFSET sqlc.arg(row_offset)::int;

-- name: DestRunCountsByStatus :many
SELECT status, count(id)::bigint AS n FROM destination_runs
WHERE workspace_id = sqlc.arg(workspace_id)::text AND destination_id = sqlc.arg(destination_id)::text
GROUP BY status;

-- name: DestRunsLatest :many
SELECT * FROM destination_runs
WHERE workspace_id = sqlc.arg(workspace_id)::text AND destination_id = sqlc.arg(destination_id)::text
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;
