-- Plan 15 WP-WBEXEC: run_workbook residual parity (output journal, provenance write-back).

-- name: SetRunWorkbookJobPayload :exec
-- output_attempts journal write (output_attempts.claim_output_attempt / record_output_result).
-- The caller has locked the row with LockOwnedRunWorkbookJob in the same transaction.
UPDATE jobs SET payload = sqlc.arg(payload)::json
WHERE id = sqlc.arg(id)::int AND type = 'run_workbook';

-- name: LockLeadFieldProvenance :one
-- Read-modify-write of leads.field_provenance (merge_field_provenance) under a row lock.
SELECT field_provenance FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true) AND id = sqlc.arg(id)::int
FOR UPDATE;
