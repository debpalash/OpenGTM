-- llm_usage_daily is a tenant table under FORCE RLS: run these inside
-- tenancy.InWorkspace. workspace_id is always passed explicitly as well.

-- name: ListLLMUsageForDate :many
SELECT provider, model, calls, total_tokens, rate_limit, rate_remaining, rate_reset
FROM llm_usage_daily
WHERE workspace_id = sqlc.arg(workspace_id)::text AND date = sqlc.arg(date)::text
ORDER BY calls DESC, provider;

-- name: LLMUsageTotals :one
SELECT COALESCE(SUM(calls), 0)::bigint AS total_calls,
       COALESCE(SUM(total_tokens), 0)::bigint AS total_tokens
FROM llm_usage_daily
WHERE workspace_id = sqlc.arg(workspace_id)::text;
