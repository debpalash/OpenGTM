-- Governance audit trail. Tenant table under FORCE RLS: run inside tenancy.InWorkspace.

-- name: InsertGovernanceAudit :exec
INSERT INTO governance_audit_events
    (id, workspace_id, actor_user_id, actor_role, method, route, resource_path,
     response_status, outcome, request_id, metadata_json)
VALUES
    (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.narg(actor_user_id)::int, sqlc.arg(actor_role)::text,
     sqlc.arg(method)::text, sqlc.arg(route)::text, sqlc.arg(resource_path)::text,
     sqlc.arg(response_status)::int, sqlc.arg(outcome)::text, sqlc.arg(request_id)::text,
     sqlc.arg(metadata_json)::json);
