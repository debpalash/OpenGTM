-- SQLite fixture standing in for the pre-Postgres lead store (v3.0.0 services/leadgen/db.py); only a table and rows.
CREATE TABLE leads (id INTEGER PRIMARY KEY, company TEXT);
INSERT INTO leads VALUES (1, 'Old Co'), (2, 'Older Co');
