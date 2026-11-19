-- SQLite fixture: DDL from the v3.0.0 tag (apps/api/routers/settings.py, _get_db).
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT DEFAULT ''
);
INSERT INTO settings (key, value) VALUES
 ('ANTHROPIC_API_KEY', 'enc:v1:gAAAAABmProviderKeyCiphertext=='), ('ACTIVE_WORKSPACE', 'ws-main'), ('EMPTY_VALUE', NULL),
 ('__stores_backend', 'postgres');
