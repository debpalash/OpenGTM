-- SQLite fixture: DDL from the v3.0.0 tag (apps/api/services/workbook/functions.py, _get_db).
CREATE TABLE IF NOT EXISTS functions (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT DEFAULT '',
    category TEXT DEFAULT 'custom',
    columns_chain TEXT DEFAULT '[]',
    version INTEGER DEFAULT 1,
    usage_count INTEGER DEFAULT 0,
    created_at REAL,
    updated_at REAL
);
INSERT INTO functions VALUES ('f1', 'Find email', 'x', 'custom', '[{"id":"a"}]', 1, 3, 1760000000.0, 1760000001.0),
 ('f2', 'Broken', NULL, NULL, 'not json', 1, 0, 1760000000.0, 1760000001.0);
