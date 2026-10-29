-- app_settings is global (no RLS): run these on the plain pool.

-- name: GetAppSetting :one
SELECT value FROM app_settings WHERE key = sqlc.arg(key)::text;

-- name: UpsertAppSetting :exec
INSERT INTO app_settings (key, value, updated_at) VALUES (sqlc.arg(key)::text, sqlc.arg(value)::text, now())
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();
