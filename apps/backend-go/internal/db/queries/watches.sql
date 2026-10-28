-- plan 08 (automations) task 13: watch CRUD, poll-now and signal feed for
-- internal/watchsvc. Every statement runs inside tenancy.InWorkspace.

-- name: WatchSvcCount :one
SELECT count(*)::bigint AS n FROM watch_subscriptions WHERE workspace_id = sqlc.arg(workspace_id)::text;

-- name: WatchSvcInsert :one
INSERT INTO watch_subscriptions (id, workspace_id, kind, target, lead_id, company_entity_id,person_entity_id, signal_types, "interval", config,
                                 schedule_anchor, next_poll_at, enabled, "cursor", consecutive_failures)
VALUES (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(kind)::text, sqlc.arg(target)::text,
        sqlc.narg(lead_id)::int4,sqlc.narg(company_id)::text,sqlc.narg(person_id)::text, sqlc.arg(signal_types)::json, sqlc.arg(poll_interval)::text,
        sqlc.narg(config)::json, sqlc.arg(schedule_anchor)::timestamptz, sqlc.narg(next_poll_at)::timestamptz, true,
        sqlc.arg(poll_cursor)::json, 0)
RETURNING *;

-- name: WatchSvcGet :one
SELECT * FROM watch_subscriptions WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: WatchSvcLock :one
SELECT * FROM watch_subscriptions WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text FOR UPDATE;

-- name: WatchSvcList :many
SELECT * FROM watch_subscriptions
WHERE workspace_id = sqlc.arg(workspace_id)::text
  AND (sqlc.narg(cursor_ts)::timestamptz IS NULL
       OR created_at < sqlc.narg(cursor_ts)::timestamptz
       OR (created_at = sqlc.narg(cursor_ts)::timestamptz AND id < sqlc.narg(cursor_id)::text))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int OFFSET sqlc.arg(row_offset)::int;

-- name: WatchSvcSave :one
-- Full read-modify-write of the fields the API edits (Python commits the ORM
-- object; updated_at has no onupdate on this model, so it is left alone).
UPDATE watch_subscriptions
SET "interval" = sqlc.narg(poll_interval)::text, config = sqlc.narg(config)::json,
    signal_types = sqlc.narg(signal_types)::json, lead_id = sqlc.narg(lead_id)::int4,company_entity_id=sqlc.narg(company_id)::text,person_entity_id=sqlc.narg(person_id)::text,
    enabled = sqlc.arg(enabled)::boolean, next_poll_at = sqlc.narg(next_poll_at)::timestamptz,
    consecutive_failures = sqlc.arg(consecutive_failures)::int4, last_error = sqlc.narg(last_error)::text
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
RETURNING *;

-- name: WatchSvcDelete :execrows
DELETE FROM watch_subscriptions WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: WatchSvcActiveScheduledJobs :many
-- Active watch_poll jobs of one watch (fire_key watch:<id>:...). The caller
-- drops the manual ":pollnow:" keys.
SELECT id, fire_key FROM jobs
WHERE type = 'watch_poll' AND starts_with(fire_key, sqlc.arg(prefix)::text) AND status IN ('pending', 'processing')
ORDER BY id;

-- name: WatchSvcLeadExists :one
SELECT EXISTS (SELECT 1 FROM record_leads WHERE id = sqlc.arg(id)::int4 AND workspace_id = sqlc.arg(workspace_id)::text) AS found;

-- name: WatchSvcSignalsPage :many
-- SignalStore.get_signals_page with the account-group identity filter
-- (lead ids OR lower(company)) on top of the plain type/lead filters.
SELECT *
FROM signals
WHERE workspace_id = sqlc.arg(workspace_id)::text
  AND (sqlc.narg(signal_types)::text[] IS NULL OR signal_type = ANY(sqlc.narg(signal_types)::text[]))
  AND (sqlc.narg(lead_id)::int4 IS NULL OR lead_id = sqlc.narg(lead_id)::int4)
  AND (NOT sqlc.arg(identity_filter)::boolean
       OR lead_id = ANY(sqlc.arg(lead_ids)::int4[])
       OR lower(company) = ANY(sqlc.arg(companies)::text[]))
  AND (sqlc.narg(cursor_ts)::float8 IS NULL
       OR created_at < sqlc.narg(cursor_ts)::float8
       OR (created_at = sqlc.narg(cursor_ts)::float8 AND id < sqlc.narg(cursor_id)::text))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int OFFSET sqlc.arg(row_offset)::int;

-- name: WatchSvcInsertWebhookRule :one
INSERT INTO triggers (id, workspace_id, name, enabled, trigger_type, trigger_config, "condition", actions, scope_workbook_ids)
VALUES (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(name)::text, true, 'on_signal',
        sqlc.arg(trigger_config)::json, '', sqlc.arg(actions)::json, '[]'::json)
RETURNING id;

-- name: WatchSvcExists :one
SELECT EXISTS (SELECT 1 FROM watch_subscriptions
               WHERE workspace_id = sqlc.arg(workspace_id)::text AND kind = sqlc.arg(kind)::text AND target = sqlc.arg(target)::text) AS found;

-- name: WatchSvcPendingPollNow :one
-- Manual polls of this workspace enqueued for the UTC day and not yet finished
-- (their quota debit happens at execution).
SELECT count(*)::int AS n FROM jobs
WHERE type = 'watch_poll' AND workspace_id = sqlc.arg(workspace_id)::text
  AND fire_key LIKE 'watch:%:pollnow:' || sqlc.arg(day)::text
  AND status IN ('pending', 'processing');

-- plan 08 task 15: watch_poll handler (internal/poller/engine.go). Tenant
-- statements run inside tenancy.InWorkspace.

-- name: PollEngineGet :one
-- Unlocked snapshot of the fields a poll reads (a fetch must not hold the row lock).
SELECT id, workspace_id, kind, target, resolved_cik, lead_id, company_entity_id,person_entity_id, signal_types, config, "cursor", enabled
FROM watch_subscriptions
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: PollEngineLock :one
-- Row-locked read of the mutable poll state, taken only to apply a source's result.
SELECT resolved_cik, "cursor", last_error, enabled
FROM watch_subscriptions
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text
FOR UPDATE;

-- name: PollEngineLeadCompanies :many
SELECT id, company FROM record_leads WHERE workspace_id = sqlc.arg(workspace_id)::text;

-- name: PollEngineLeadWebsite :one
SELECT website FROM record_leads WHERE id = sqlc.arg(id)::int4 AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: PollEngineSaveSource :exec
-- One source's outcome: merged cursor, resolved_cik and last_error (all computed
-- from the locked row), written in the same transaction as the emitted signals.
UPDATE watch_subscriptions
SET "cursor" = sqlc.arg(poll_cursor)::json, resolved_cik = sqlc.narg(resolved_cik)::text,
    last_error = sqlc.narg(last_error)::text
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: PollEngineSaveCursor :exec
UPDATE watch_subscriptions SET "cursor" = sqlc.arg(poll_cursor)::json
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: PollEngineSetPolled :exec
UPDATE watch_subscriptions SET last_polled_at = sqlc.arg(polled_at)::timestamptz
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: PollEngineMirrorDelete :exec
-- mirror_delete scoped to the job's workspace: a job naming a watch of another
-- workspace must not drop that watch's schedule row (the mirror is not RLS).
DELETE FROM watch_schedules WHERE watch_id = sqlc.arg(watch_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;
