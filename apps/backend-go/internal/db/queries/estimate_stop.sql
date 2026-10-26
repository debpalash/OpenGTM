-- Queries for the estimate / stop / provider-stats routes (Task 23).

-- name: StopCancelRunJobs :execrows
-- workbooks.py stop_workbook: every pending/processing run_workbook job of the
-- workbook is cancelled. The workspace predicate is explicit on top of RLS.
UPDATE jobs SET status = 'cancelled', error = 'stopped by user'
WHERE type = 'run_workbook'
  AND status IN ('pending', 'processing')
  AND workspace_id = sqlc.arg(workspace_id)::text
  AND payload->>'workbook_id' = sqlc.arg(workbook_id)::text;

-- name: SetWorkbookPaused :execrows
UPDATE workbooks SET status = 'paused', updated_at = (now() AT TIME ZONE 'utc')
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: ListProviderStats :many
-- GET /meta/provider-stats: attempts desc (NULLs first, like PostgreSQL's DESC
-- default Python relies on); ties in insertion order (the unspecified order
-- Python gets from a heap scan).
SELECT provider, field, attempts, hits, total_confidence, total_latency_ms, total_cost_usd,
       cooldown_until, accuracy_score, accuracy_samples
FROM provider_stats
ORDER BY attempts DESC, id;
