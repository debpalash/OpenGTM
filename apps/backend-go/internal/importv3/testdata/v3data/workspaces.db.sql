-- SQLite fixture: the DDL below is copied verbatim from the v3.0.0 tag (apps/api/services/workspace/manager.py, _get_db).
        CREATE TABLE IF NOT EXISTS workspaces (
            id TEXT PRIMARY KEY,
            name TEXT NOT NULL,
            slug TEXT UNIQUE NOT NULL,
            description TEXT DEFAULT '',
            icon TEXT DEFAULT '🏢',
            owner_id INTEGER,
            created_at REAL,
            updated_at REAL
        );

        CREATE TABLE IF NOT EXISTS workspace_settings (
            workspace_id TEXT NOT NULL,
            key TEXT NOT NULL,
            value TEXT DEFAULT '',
            PRIMARY KEY (workspace_id, key),
            FOREIGN KEY (workspace_id) REFERENCES workspaces(id)
        );

        -- Tenant membership: which users belong to which workspaces
        CREATE TABLE IF NOT EXISTS workspace_members (
            workspace_id TEXT NOT NULL,
            user_id INTEGER NOT NULL,
            role TEXT DEFAULT 'member',  -- owner, admin, member, viewer
            created_at REAL,
            PRIMARY KEY (workspace_id, user_id),
            FOREIGN KEY (workspace_id) REFERENCES workspaces(id)
        );

        CREATE TABLE IF NOT EXISTS workspace_member_permissions (
            workspace_id TEXT NOT NULL,
            user_id INTEGER NOT NULL,
            permission TEXT NOT NULL,
            effect TEXT NOT NULL CHECK(effect IN ('allow', 'deny')),
            updated_at REAL NOT NULL,
            PRIMARY KEY (workspace_id, user_id, permission),
            FOREIGN KEY (workspace_id, user_id) REFERENCES workspace_members(workspace_id, user_id)
        );

        CREATE TABLE IF NOT EXISTS workspace_oidc_identities (
            workspace_id TEXT NOT NULL,
            issuer TEXT NOT NULL,
            subject TEXT NOT NULL,
            user_id INTEGER NOT NULL,
            email TEXT DEFAULT '',
            created_at REAL NOT NULL,
            PRIMARY KEY (workspace_id, issuer, subject),
            UNIQUE (workspace_id, user_id),
            FOREIGN KEY (workspace_id, user_id) REFERENCES workspace_members(workspace_id, user_id)
        );

        CREATE TABLE IF NOT EXISTS workspace_scim_tokens (
            workspace_id TEXT PRIMARY KEY,
            token_hash TEXT NOT NULL,
            token_prefix TEXT NOT NULL,
            created_at REAL NOT NULL,
            expires_at REAL,
            last_used_at REAL,
            created_by INTEGER,
            FOREIGN KEY (workspace_id) REFERENCES workspaces(id)
        );

        CREATE TABLE IF NOT EXISTS workspace_scim_users (
            workspace_id TEXT NOT NULL,
            user_id INTEGER NOT NULL,
            external_id TEXT DEFAULT '',
            display_name TEXT DEFAULT '',
            active INTEGER NOT NULL DEFAULT 1,
            created_at REAL NOT NULL,
            updated_at REAL NOT NULL,
            PRIMARY KEY (workspace_id, user_id),
            FOREIGN KEY (workspace_id) REFERENCES workspaces(id)
        );

        CREATE TABLE IF NOT EXISTS workspace_scim_groups (
            id TEXT PRIMARY KEY,
            workspace_id TEXT NOT NULL,
            external_id TEXT DEFAULT '',
            display_name TEXT NOT NULL,
            created_at REAL NOT NULL,
            updated_at REAL NOT NULL,
            UNIQUE (workspace_id, display_name),
            FOREIGN KEY (workspace_id) REFERENCES workspaces(id)
        );

        CREATE TABLE IF NOT EXISTS workspace_scim_group_members (
            workspace_id TEXT NOT NULL,
            group_id TEXT NOT NULL,
            user_id INTEGER NOT NULL,
            created_at REAL NOT NULL,
            PRIMARY KEY (workspace_id, group_id, user_id),
            FOREIGN KEY (group_id) REFERENCES workspace_scim_groups(id)
        );

        -- Per-user active workspace (replaces the global ACTIVE_WORKSPACE setting)
        CREATE TABLE IF NOT EXISTS user_active_workspace (
            user_id INTEGER PRIMARY KEY,
            workspace_id TEXT NOT NULL
        );

INSERT INTO workspaces (id, name, slug, description, icon, owner_id, created_at, updated_at) VALUES
 ('ws-main', 'Default Workspace', 'main', 'Your main workspace', '🏠', NULL, 1760000000.5, 1760000000.5),
 ('ws-acme', 'Acme', 'acme', NULL, NULL, 1, 1760000100.0, 1760000100.0);
INSERT INTO workspace_members (workspace_id, user_id, role, created_at) VALUES
 ('ws-main', 1, 'owner', 1760000001.0), ('ws-acme', 1, 'owner', 1760000101.0), ('ws-acme', 2, NULL, 1760000102.0),
 ('ws-acme', 99, 'member', 1760000103.0);
INSERT INTO user_active_workspace (user_id, workspace_id) VALUES (1, 'ws-main'), (2, 'ws-acme');
INSERT INTO workspace_settings (workspace_id, key, value) VALUES
 ('ws-main', 'hubspot_token', 'enc:v1:gAAAAABmRehearsalCiphertext=='), ('ws-acme', 'smtp_host', 'smtp.example.com'), ('ws-acme', 'empty', NULL);
INSERT INTO workspace_member_permissions (workspace_id, user_id, permission, effect, updated_at) VALUES ('ws-acme', 2, 'export', 'deny', 1760000200.0);
INSERT INTO workspace_oidc_identities (workspace_id, issuer, subject, user_id, email, created_at) VALUES ('ws-acme', 'https://idp.example.com', 'sub-2', 2, NULL, 1760000300.0);
INSERT INTO workspace_scim_tokens (workspace_id, token_hash, token_prefix, created_at, expires_at, last_used_at, created_by) VALUES ('ws-acme', 'hash', 'scim_ab', 1760000400.0, NULL, NULL, 1);
INSERT INTO workspace_scim_users (workspace_id, user_id, external_id, display_name, active, created_at, updated_at) VALUES ('ws-acme', 2, NULL, NULL, 0, 1760000500.0, 1760000500.0);
INSERT INTO workspace_scim_groups (id, workspace_id, external_id, display_name, created_at, updated_at) VALUES ('grp-1', 'ws-acme', NULL, 'Sales', 1760000600.0, 1760000600.0);
INSERT INTO workspace_scim_group_members (workspace_id, group_id, user_id, created_at) VALUES ('ws-acme', 'grp-1', 2, 1760000700.0);
