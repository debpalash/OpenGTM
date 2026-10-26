-- Copilot builtin memory store (plan 12 Task 4). chat_memories has no RLS and no
-- workspace column: the caller namespaces user_id as (workspace, user).

-- name: MemAdd :exec
INSERT INTO chat_memories (user_id, text, metadata, created_at)
VALUES (@user_id, @text, @metadata::jsonb, @created_at);

-- name: MemRecent :many
SELECT text, metadata, created_at FROM chat_memories
WHERE user_id = @user_id
ORDER BY id DESC
LIMIT 500;

-- name: MemAll :many
SELECT text, metadata, created_at FROM chat_memories
WHERE user_id = @user_id
ORDER BY id DESC;
