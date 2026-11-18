-- First-run checklist (GET /api/onboarding). Both queries run inside tenancy.InWorkspace; the explicit
-- workspace_id filter is belt and braces on top of FORCE RLS.

-- name: OnboardingProgress :one
SELECT
  (EXISTS (SELECT 1 FROM workbooks WHERE workspace_id = sqlc.arg(workspace_id)::text)
   OR EXISTS (SELECT 1 FROM leads WHERE workspace_id = sqlc.arg(workspace_id)::text))::boolean AS has_data,
  EXISTS (SELECT 1 FROM workbook_enrichments
          WHERE workspace_id = sqlc.arg(workspace_id)::text AND status = 'complete')::boolean AS has_enrichment;

-- name: OnboardingWorkbookExists :one
SELECT EXISTS (SELECT 1 FROM workbooks WHERE workspace_id = sqlc.arg(workspace_id)::text AND id = sqlc.arg(id)::text)::boolean AS found;
