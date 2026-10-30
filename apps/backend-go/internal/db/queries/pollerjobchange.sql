-- Workspace-scoped lead lookup for job_change contact materialisation
-- (job_change.materialize_contacts). Runs inside tenancy.InWorkspace; the
-- explicit workspace_id predicate mirrors the Python filter.

-- name: PollContactLead :one
SELECT contact_person, company, linkedin_url
FROM leads
WHERE id = $1 AND workspace_id = $2;
