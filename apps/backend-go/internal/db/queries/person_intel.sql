-- LinkedIn person intel records (plan 12 Task 4). Global table: no workspace
-- column, no RLS; routes are admin only.

-- name: PersonInsert :one
INSERT INTO person_intel (linkedin_url, username, name, headline, location, summary, emails,
                          social_links, articles, mentions, companies, education, skills,
                          raw_sources, status, created_at)
VALUES (@linkedin_url, @username, @name, @headline, @location, @summary, @emails,
        @social_links, @articles, @mentions, @companies, @education, @skills,
        @raw_sources, @status, @created_at)
RETURNING id;

-- name: PersonHistory :many
SELECT id, linkedin_url, username, name, headline, location, summary, emails, social_links,
       articles, mentions, companies, education, skills, raw_sources, status, created_at, updated_at
FROM person_intel
ORDER BY id DESC
OFFSET @row_skip LIMIT @row_limit;

-- name: PersonGet :one
SELECT id, linkedin_url, username, name, headline, location, summary, emails, social_links,
       articles, mentions, companies, education, skills, raw_sources, status, created_at, updated_at
FROM person_intel
WHERE id = @id;

-- name: PersonDelete :execrows
DELETE FROM person_intel WHERE id = @id;
