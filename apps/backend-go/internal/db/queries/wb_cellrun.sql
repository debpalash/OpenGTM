-- Single-cell run route (plan 06 Task 25). Every statement runs inside tenancy.InWorkspace.

-- name: GetWorkbookForCellRun :one
SELECT id, columns_config, source_type, source_config, filter_criteria
FROM workbooks
WHERE id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: GetEnrichmentForOutputGuard :one
-- The (workbook, lead, column) overlay row an output run-once guard consults.
SELECT status, value, provider FROM workbook_enrichments
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
  AND lead_id = sqlc.arg(lead_id)::int AND column_id = sqlc.arg(column_id)::text
ORDER BY id LIMIT 1;

-- name: ActiveRunWorkbookJobID :one
-- The live (processing first, else newest pending) run_workbook job of a workbook: an in-request
-- cell run reserves spend under that job's attempt keys so a race with the queued run settles once.
SELECT id FROM jobs
WHERE type = 'run_workbook' AND workspace_id = sqlc.arg(workspace_id)::text
  AND status IN ('pending', 'processing')
  AND CASE WHEN json_typeof(payload) = 'object' THEN payload ->> 'workbook_id' END = sqlc.arg(workbook_id)::text
ORDER BY (status = 'processing') DESC, id DESC
LIMIT 1;
