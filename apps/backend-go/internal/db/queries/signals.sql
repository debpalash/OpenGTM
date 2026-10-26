-- Signals store reads and mark-read (port of services/signals/store.py and the
-- analytics route in routers/signals.py). `signals` is FORCE RLS: run inside
-- tenancy.InWorkspace. Go never inserts signals (the Python scanner owns that).
-- Nullable columns are COALESCEd so the Go Signal struct stays plain-typed;
-- the Python scanner always writes them.

-- name: ListSignals :many
SELECT id, workspace_id, lead_id, COALESCE(company, '')::text AS company,
       COALESCE(signal_type, '')::text AS signal_type, COALESCE(title, '')::text AS title,
       COALESCE(description, '')::text AS description, COALESCE(source, '')::text AS source,
       COALESCE(source_url, '')::text AS source_url, weight,
       COALESCE(created_at, 0)::float8 AS created_at, COALESCE(read, false)::bool AS read
FROM signals
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND (sqlc.narg('signal_type')::text IS NULL OR signal_type = sqlc.narg('signal_type')::text)
  AND (sqlc.narg('lead_id')::int4 IS NULL OR lead_id = sqlc.narg('lead_id')::int4)
ORDER BY created_at DESC, id DESC
LIMIT @page_limit OFFSET @page_offset;

-- name: PageSignals :many
SELECT id, workspace_id, lead_id, COALESCE(company, '')::text AS company,
       COALESCE(signal_type, '')::text AS signal_type, COALESCE(title, '')::text AS title,
       COALESCE(description, '')::text AS description, COALESCE(source, '')::text AS source,
       COALESCE(source_url, '')::text AS source_url, weight,
       COALESCE(created_at, 0)::float8 AS created_at, COALESCE(read, false)::bool AS read
FROM signals
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND (sqlc.narg('signal_types')::text[] IS NULL OR signal_type = ANY(sqlc.narg('signal_types')::text[]))
  AND (sqlc.narg('lead_id')::int4 IS NULL OR lead_id = sqlc.narg('lead_id')::int4)
  AND (sqlc.narg('cursor_ts')::float8 IS NULL
       OR created_at < sqlc.narg('cursor_ts')::float8
       OR (created_at = sqlc.narg('cursor_ts')::float8 AND id < sqlc.narg('cursor_id')::text))
ORDER BY created_at DESC, id DESC
LIMIT @page_limit;

-- name: SignalTypeCounts :many
SELECT signal_type, count(*)::bigint AS n
FROM signals
WHERE workspace_id = current_setting('app.workspace_id', true)
GROUP BY signal_type;

-- name: SignalTotals :one
SELECT count(*)::bigint AS total,
       (count(*) FILTER (WHERE read IS NOT TRUE))::bigint AS unread
FROM signals
WHERE workspace_id = current_setting('app.workspace_id', true);

-- name: MarkSignalsRead :exec
UPDATE signals SET read = true
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND id = ANY(@ids::text[]);

-- name: SignalAnalyticsSummary :one
SELECT count(id)::bigint AS total,
       COALESCE(sum(weight), 0)::bigint AS weighted,
       count(DISTINCT lead_id)::bigint AS accounts
FROM signals
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND created_at >= @cutoff::float8;

-- name: SignalAnalyticsByType :many
SELECT signal_type, count(id)::bigint AS n, COALESCE(sum(weight), 0)::bigint AS weight
FROM signals
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND created_at >= @cutoff::float8
GROUP BY signal_type
ORDER BY count(id) DESC, signal_type;

-- name: SignalAnalyticsBySource :many
SELECT source, count(id)::bigint AS n
FROM signals
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND created_at >= @cutoff::float8
GROUP BY source
ORDER BY count(id) DESC, source
LIMIT 10;

-- name: SignalAnalyticsTopAccounts :many
SELECT lead_id, company, count(id)::bigint AS n, COALESCE(sum(weight), 0)::bigint AS weight
FROM signals
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND created_at >= @cutoff::float8
GROUP BY lead_id, company
ORDER BY sum(weight) DESC, count(id) DESC, lead_id, company
LIMIT 10;

-- Parity quirk kept verbatim: CAST(float8 AS INTEGER) ROUNDS (half-even), it does
-- not floor, so signals in the second half of a UTC day land in the next bucket.
-- name: SignalAnalyticsDayBuckets :many
SELECT CAST(created_at / 86400 AS INTEGER)::int4 AS day, count(id)::bigint AS n,
       COALESCE(sum(weight), 0)::bigint AS weight
FROM signals
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND created_at >= @cutoff::float8
GROUP BY CAST(created_at / 86400 AS INTEGER);

-- name: SignalAnalyticsPrevious :one
SELECT count(id)::bigint
FROM signals
WHERE workspace_id = current_setting('app.workspace_id', true)
  AND created_at >= @previous_cutoff::float8 AND created_at < @cutoff::float8;
