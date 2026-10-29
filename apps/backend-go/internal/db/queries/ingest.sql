-- Inbound rows (routers/ingest.py + services/workbook/ingest.py). workbook_ingest_tokens is the
-- AUTH-PLANE table (no RLS): looked up by sha256 hash before any workspace is bound. The idempotency
-- ledger is RLS-scoped (fail-closed on app.workspace_id).

-- name: GetIngestTokenByHash :one
SELECT id, workspace_id, workbook_id, token_hash, revoked_at
FROM workbook_ingest_tokens
WHERE token_hash = sqlc.arg(token_hash)::text
LIMIT 1;

-- name: RevokeActiveIngestTokens :execrows
UPDATE workbook_ingest_tokens SET revoked_at = now()
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text AND revoked_at IS NULL;

-- name: InsertIngestToken :one
INSERT INTO workbook_ingest_tokens (id, workspace_id, workbook_id, token_hash, prefix, created_by)
VALUES (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(workbook_id)::text, sqlc.arg(token_hash)::text,
        sqlc.arg(prefix)::text, sqlc.narg(created_by)::int)
RETURNING id, created_at;

-- name: GetIngestIdempotency :one
SELECT response::text AS response FROM workbook_ingest_idempotency
WHERE workbook_id = sqlc.arg(workbook_id)::text AND idempotency_key = sqlc.arg(idempotency_key)::text;

-- name: InsertIngestIdempotency :execrows
INSERT INTO workbook_ingest_idempotency (id, workspace_id, workbook_id, idempotency_key, response)
VALUES (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.arg(workbook_id)::text, sqlc.arg(idempotency_key)::text,
        sqlc.arg(response)::text::json)
ON CONFLICT (workbook_id, idempotency_key) DO NOTHING;

-- name: PruneIngestIdempotency :exec
DELETE FROM workbook_ingest_idempotency
WHERE workbook_id = sqlc.arg(workbook_id)::text AND created_at < now() - make_interval(days => sqlc.arg(retention_days)::int);

-- name: SetWorkbookTotalRows :exec
UPDATE workbooks SET total_rows = sqlc.arg(total_rows)::int, updated_at = (now() AT TIME ZONE 'utc')
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;
