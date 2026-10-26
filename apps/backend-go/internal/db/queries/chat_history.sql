-- Copilot chat history: conversations, messages, tool approvals (plan 12 Task 4).
-- Tenant tables under FORCE RLS: run inside tenancy.InWorkspace; every query also
-- filters workspace_id (and user_id) explicitly, as Python's _pg_* helpers do.
-- Timestamps are formatted like chat_history.py _pg_ts: UTC 'YYYY-MM-DD HH24:MI:SS'.

-- name: ChatCreateConversation :exec
INSERT INTO chat_conversations (id, title, workspace_id, user_id)
VALUES (@id, @title, @workspace_id, @user_id);

-- name: ChatListConversations :many
SELECT id, title,
       to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS')::text AS created_at,
       to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS')::text AS updated_at
FROM chat_conversations
WHERE workspace_id = @workspace_id AND user_id = @user_id
ORDER BY chat_conversations.updated_at DESC
LIMIT @row_limit;

-- name: ChatGetConversation :one
SELECT id, title,
       to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS')::text AS created_at,
       to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS')::text AS updated_at
FROM chat_conversations
WHERE id = @id AND workspace_id = @workspace_id AND user_id = @user_id;

-- name: ChatGetMessages :many
SELECT m.id, m.conversation_id, m.role, m.content, m.tool_data,
       to_char(m.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS')::text AS created_at
FROM chat_messages m
JOIN chat_conversations c ON c.id = m.conversation_id
WHERE m.conversation_id = @conversation_id AND c.workspace_id = @workspace_id AND c.user_id = @user_id
ORDER BY m.created_at ASC;

-- Inserts the message only while the conversation exists for (workspace, user)
-- and bumps the conversation's updated_at in the same statement. No row back
-- (pgx.ErrNoRows) means no such conversation (Python returns None).
-- name: ChatAddMessage :one
WITH ins AS (
  INSERT INTO chat_messages (id, conversation_id, workspace_id, role, content, tool_data)
  SELECT @id::text, c.id, c.workspace_id, @role::text, @content::text, sqlc.narg(tool_data)::text
  FROM chat_conversations c
  WHERE c.id = @conversation_id AND c.workspace_id = @workspace_id AND c.user_id = @user_id
  RETURNING id, conversation_id
), bump AS (
  UPDATE chat_conversations SET updated_at = now()
  WHERE id IN (SELECT conversation_id FROM ins) AND workspace_id = @workspace_id
)
SELECT id FROM ins;

-- name: ChatUpdateTitle :exec
UPDATE chat_conversations SET title = @title
WHERE id = @id AND workspace_id = @workspace_id AND user_id = @user_id;

-- name: ChatDeleteConversation :exec
DELETE FROM chat_conversations
WHERE id = @id AND workspace_id = @workspace_id AND user_id = @user_id;

-- name: ChatCreateApproval :exec
INSERT INTO chat_tool_approvals (id, workspace_id, user_id, tool_call_json, status, created_at_epoch)
VALUES (@id, @workspace_id, @user_id, @tool_call_json, 'pending', @created_at_epoch);

-- Single atomic, single-use consume: pending guard, tenant match, TTL via
-- min_created_at_epoch (= now - ttl). No row back means unknown, foreign,
-- consumed or expired.
-- name: ChatConsumeApproval :one
UPDATE chat_tool_approvals SET status = @decision
WHERE id = @id AND workspace_id = @workspace_id AND user_id = @user_id
  AND status = 'pending' AND created_at_epoch >= @min_created_at_epoch
RETURNING tool_call_json;
