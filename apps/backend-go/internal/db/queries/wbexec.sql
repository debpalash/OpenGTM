-- Plan 15 WP-WBEXEC: run_workbook residual parity (output journal, provenance write-back).

-- name: SetRunWorkbookJobPayload :exec
-- output_attempts journal write (output_attempts.claim_output_attempt / record_output_result).
-- The caller has locked the row with LockOwnedRunWorkbookJob in the same transaction.
UPDATE jobs SET payload = sqlc.arg(payload)::json
WHERE id = sqlc.arg(id)::int AND type = 'run_workbook' AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: LockLeadFieldProvenance :one
-- Read-modify-write of leads.field_provenance (merge_field_provenance) under a row lock.
SELECT field_provenance FROM record_leads
WHERE workspace_id = current_setting('app.workspace_id', true) AND id = sqlc.arg(id)::int
FOR UPDATE;

-- name: ProcessingRunWorkbookJobID :one
-- A run_workbook job of the workbook that a worker is processing right now (M2: a single-cell
-- output run is refused while it delivers the same cells under its own journal).
SELECT id FROM jobs
WHERE type = 'run_workbook' AND workspace_id = sqlc.arg(workspace_id)::text AND status = 'processing'
  AND CASE WHEN json_typeof(payload) = 'object' THEN payload ->> 'workbook_id' END = sqlc.arg(workbook_id)::text
LIMIT 1;

-- name: GetOutputCellClaim :one
-- The overlay row an in-request output run claims (status running + cell_metadata.output_claim).
SELECT status, COALESCE(cell_metadata::text, '')::text AS cell_metadata FROM workbook_enrichments
WHERE workbook_id = sqlc.arg(workbook_id)::text AND workspace_id = sqlc.arg(workspace_id)::text
  AND lead_id = sqlc.arg(lead_id)::int AND column_id = sqlc.arg(column_id)::text
ORDER BY id LIMIT 1
FOR UPDATE;
