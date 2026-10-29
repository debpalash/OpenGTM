-- MCP audit log (plan 13, task 6). mcp_audit_log is a tenant table under FORCE
-- RLS: run inside tenancy.InWorkspace; the WITH CHECK policy stamps the tenant.

-- name: McpAuditInsert :exec
INSERT INTO mcp_audit_log
    (id, workspace_id, token_id, user_id, tool_name, arguments_redacted,
     idempotency_key, result_status, error, estimated_cost_usd)
VALUES
    (sqlc.arg(id)::text, sqlc.arg(workspace_id)::text, sqlc.narg(token_id)::text, sqlc.narg(user_id)::int,
     sqlc.arg(tool_name)::text, sqlc.arg(arguments_redacted)::json,
     sqlc.narg(idempotency_key)::text, sqlc.arg(result_status)::text, sqlc.narg(error)::text,
     sqlc.arg(estimated_cost_usd)::float8);
