-- Enrichment provider cache and email-verify cache queries (workspace-scoped, RLS).
-- Queries are added by Task 11 (internal/enrichcache). Tenant tables under FORCE
-- RLS: run inside tenancy.InWorkspace. Timestamps are naive UTC.

-- name: EnrichCacheGet :one
SELECT value, provider, confidence
FROM enrichment_cache
WHERE workspace_id = $1 AND cache_key = $2 AND field_name = $3
  AND expires_at >= $4;

-- name: EnrichCachePut :exec
INSERT INTO enrichment_cache (workspace_id, cache_key, field_name, value, provider, confidence, cached_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (workspace_id, cache_key, field_name) DO UPDATE SET
  value = EXCLUDED.value,
  provider = EXCLUDED.provider,
  confidence = EXCLUDED.confidence,
  cached_at = EXCLUDED.cached_at,
  expires_at = EXCLUDED.expires_at;

-- name: EnrichCachePurge :execrows
DELETE FROM enrichment_cache WHERE workspace_id = $1 AND expires_at < $2;

-- name: EmailVerifyCacheGet :one
SELECT result
FROM email_verify_cache
WHERE workspace_id = $1 AND email_hash = $2 AND expires_at >= $3;

-- name: EmailVerifyCachePut :exec
INSERT INTO email_verify_cache (workspace_id, email_hash, result, cached_at, expires_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (workspace_id, email_hash) DO UPDATE SET
  result = EXCLUDED.result,
  cached_at = EXCLUDED.cached_at,
  expires_at = EXCLUDED.expires_at;

-- name: EmailVerifyCachePurge :execrows
DELETE FROM email_verify_cache WHERE workspace_id = $1 AND expires_at < $2;
