-- SQLite fixture: DDL from the v3.0.0 tag (apps/api/services/chat_history.py, _init_tables).
CREATE TABLE IF NOT EXISTS conversations (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL DEFAULT 'New Chat',
    workspace_id TEXT,
    user_id TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK(role IN ('user', 'assistant', 'system', 'tool')),
    content TEXT NOT NULL DEFAULT '',
    tool_data TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS tool_approvals (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    tool_call_json TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    created_at_epoch INTEGER NOT NULL
);
INSERT INTO conversations (id, title, workspace_id, user_id, created_at, updated_at) VALUES
 ('c1', 'Find CTOs', 'ws-main', '1', '2026-10-01 10:00:00', '2026-10-01 10:05:00'),
 ('c2', 'Iso chat', 'ws-acme', '2', '2026-10-02T09:00:00+00:00', '2026-10-02T09:00:00.123456+00:00'),
 ('c-legacy', 'Before tenancy', NULL, NULL, '2026-09-01 08:00:00', '2026-09-01 08:00:00');
INSERT INTO messages (id, conversation_id, role, content, tool_data, created_at) VALUES
 ('m1', 'c1', 'user', 'hello', NULL, '2026-10-01 10:00:00'),
 ('m2', 'c1', 'assistant', 'hi', '{"x":1}', '2026-10-01 10:00:00'),
 ('m3', 'c2', 'user', 'iso', NULL, '2026-10-02T09:00:00+00:00'),
 ('m-orphan', 'c-legacy', 'user', 'private', NULL, '2026-09-01 08:00:00');
INSERT INTO tool_approvals (id, workspace_id, user_id, tool_call_json, status, created_at_epoch) VALUES
 ('a1', 'ws-main', '1', '{"tool":"send"}', 'pending', 1760000000);
