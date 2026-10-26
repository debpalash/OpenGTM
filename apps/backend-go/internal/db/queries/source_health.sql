-- plan 14: source_health is GLOBAL (no workspace_id, no RLS; sources are shared
-- infrastructure). Timestamps are naive (timestamp without time zone), as in
-- the Python ORM model.

-- name: SHAll :many
SELECT name, state, consecutive_zero, consecutive_nonzero, last_yield, ewma_yield, probes, last_probe_at, last_ok_at, last_outage_at, disabled_at, disabled_reason, manual_override, updated_at
FROM source_health
ORDER BY name;

-- name: SHGet :one
SELECT name, state, consecutive_zero, consecutive_nonzero, last_yield, ewma_yield, probes, last_probe_at, last_ok_at, last_outage_at, disabled_at, disabled_reason, manual_override, updated_at
FROM source_health
WHERE name = sqlc.arg(name)::text;

-- name: SHCreateIfMissing :exec
INSERT INTO source_health (name, state, consecutive_zero, consecutive_nonzero, probes, manual_override, updated_at)
VALUES (sqlc.arg(name)::text, 'healthy', 0, 0, 0, false, now())
ON CONFLICT (name) DO NOTHING;

-- name: SHUpsert :exec
INSERT INTO source_health (name, state, consecutive_zero, consecutive_nonzero, last_yield, ewma_yield, probes,
                           last_probe_at, last_ok_at, last_outage_at, disabled_at, disabled_reason, manual_override, updated_at)
VALUES (sqlc.arg(name)::text, sqlc.arg(state)::text, sqlc.arg(consecutive_zero)::int4, sqlc.arg(consecutive_nonzero)::int4,
        sqlc.narg(last_yield)::int4, sqlc.narg(ewma_yield)::float8, sqlc.arg(probes)::int4,
        sqlc.narg(last_probe_at)::timestamp, sqlc.narg(last_ok_at)::timestamp, sqlc.narg(last_outage_at)::timestamp,
        sqlc.narg(disabled_at)::timestamp, sqlc.narg(disabled_reason)::text, sqlc.arg(manual_override)::bool, now())
ON CONFLICT (name) DO UPDATE SET
  state = EXCLUDED.state, consecutive_zero = EXCLUDED.consecutive_zero,
  consecutive_nonzero = EXCLUDED.consecutive_nonzero, last_yield = EXCLUDED.last_yield,
  ewma_yield = EXCLUDED.ewma_yield, probes = EXCLUDED.probes, last_probe_at = EXCLUDED.last_probe_at,
  last_ok_at = EXCLUDED.last_ok_at, last_outage_at = EXCLUDED.last_outage_at,
  disabled_at = EXCLUDED.disabled_at, disabled_reason = EXCLUDED.disabled_reason,
  manual_override = EXCLUDED.manual_override, updated_at = now();

-- name: SHManualEnable :one
-- Operator force-enable (manual_enable): pins manual_override, clears an
-- auto_disabled state. Creates a pinned row when none exists.
INSERT INTO source_health (name, state, consecutive_zero, consecutive_nonzero, probes, manual_override, updated_at)
VALUES (sqlc.arg(name)::text, 'healthy', 0, 0, 0, true, now())
ON CONFLICT (name) DO UPDATE SET
  manual_override = true,
  state = CASE WHEN source_health.state = 'auto_disabled' THEN 'healthy' ELSE source_health.state END,
  disabled_at = CASE WHEN source_health.state = 'auto_disabled' THEN NULL ELSE source_health.disabled_at END,
  disabled_reason = CASE WHEN source_health.state = 'auto_disabled' THEN NULL ELSE source_health.disabled_reason END,
  consecutive_zero = CASE WHEN source_health.state = 'auto_disabled' THEN 0 ELSE source_health.consecutive_zero END,
  updated_at = now()
RETURNING name, state, consecutive_zero, consecutive_nonzero, last_yield, ewma_yield, probes, last_probe_at, last_ok_at, last_outage_at, disabled_at, disabled_reason, manual_override, updated_at;

-- name: SHReset :one
-- Operator reset to a clean healthy baseline; no row means no-op (pgx.ErrNoRows).
UPDATE source_health
SET state = 'healthy', consecutive_zero = 0, consecutive_nonzero = 0, disabled_at = NULL,
    disabled_reason = NULL, manual_override = false, updated_at = now()
WHERE name = sqlc.arg(name)::text
RETURNING name, state, consecutive_zero, consecutive_nonzero, last_yield, ewma_yield, probes, last_probe_at, last_ok_at, last_outage_at, disabled_at, disabled_reason, manual_override, updated_at;

-- name: SHGetForUpdate :one
-- Row lock for the per-run read-modify-write (ApplyRun), so concurrent runs
-- serialize on the row instead of losing updates.
SELECT name, state, consecutive_zero, consecutive_nonzero, last_yield, ewma_yield, probes, last_probe_at, last_ok_at, last_outage_at, disabled_at, disabled_reason, manual_override, updated_at
FROM source_health
WHERE name = sqlc.arg(name)::text
FOR UPDATE;

-- name: SHDisabledNames :many
SELECT name FROM source_health WHERE state = 'auto_disabled' ORDER BY name;
