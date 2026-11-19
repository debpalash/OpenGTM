-- SQLite fixture: DDL from the v3.0.0 tag (apps/api/services/memory.py, _BuiltinMemory).
CREATE TABLE IF NOT EXISTS memories (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    TEXT NOT NULL,
    text       TEXT NOT NULL,
    metadata   TEXT DEFAULT '{}',
    created_at TEXT NOT NULL
);
INSERT INTO memories (id, user_id, text, metadata, created_at) VALUES
 (1, '1', 'prefers fintech', '{"k":"v"}', '2026-10-01T10:00:00+00:00'), (7, '1', 'EU only', NULL, '2026-10-02T10:00:00.5+00:00');
