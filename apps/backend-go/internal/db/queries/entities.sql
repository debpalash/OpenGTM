-- company_entities / entity_review_pairs / entity_merge_log are tenant tables under
-- FORCE RLS: run these inside tenancy.InWorkspace. workspace_id is also passed explicitly.

-- name: ListCompanyEntities :many
SELECT * FROM company_entities
WHERE workspace_id = sqlc.arg(workspace_id)::text
  AND corroboration_count >= sqlc.arg(min_corroboration)::int
ORDER BY corroboration_count DESC, id
LIMIT sqlc.arg(row_limit)::int;

-- name: GetCompanyEntity :one
SELECT * FROM company_entities
WHERE id = sqlc.arg(id)::text AND workspace_id = sqlc.arg(workspace_id)::text;

-- name: ListEntityReviewPairs :many
SELECT p.id, p.entity_id, p.candidate, p.score, p.reason, p.status
FROM entity_review_pairs p
JOIN company_entities e ON e.id = p.entity_id
WHERE e.workspace_id = sqlc.arg(workspace_id)::text
  AND p.status = sqlc.arg(status)::text
ORDER BY p.score DESC, p.id
LIMIT sqlc.arg(row_limit)::int;
