-- Bridge: Alembic 3f4051627384 (the head of the v3.0.0 Python release) -> e7b2c4d6f8a0 (the baseline).
--
-- Rendered with `alembic upgrade 3f4051627384:e7b2c4d6f8a0 --sql` from the legacy Python migrations (eight pure-DDL
-- revisions: 4a5162738495, 7a087faeed5b, e1f5a9b3d7c2, f2a6b0c4d8e3, a7c1e9d3b5f2, c9d2e4f6a8b1, d3f5a7b9c1e2,
-- e7b2c4d6f8a0; none contains a data migration), then edited only as follows:
--   - BEGIN/COMMIT and the alembic_version bookkeeping are removed (bridge.go runs the whole file in one
--     transaction and stamps e7b2c4d6f8a0 itself);
--   - `CREATE INDEX CONCURRENTLY` (revision e7b2c4d6f8a0, run outside a transaction by Alembic) is a plain
--     CREATE INDEX so it fits the transaction; the resulting schema is identical;
--   - the application role name yupcha_app is the {{APP_ROLE}} placeholder (YUPCHA_APP_DB_ROLE).
-- Never edit by hand. The schema produced must stay identical to the real Alembic chain
-- (TestBridgeSchemaMatchesAlembicChain compares a pg_dump --schema-only of both).

-- Running upgrade 3f4051627384 -> 4a5162738495

ALTER TABLE jobs ADD COLUMN executor TEXT DEFAULT 'python' NOT NULL;

ALTER TABLE jobs ADD COLUMN lease_token BIGINT DEFAULT '0' NOT NULL;

ALTER TABLE jobs ADD CONSTRAINT ck_jobs_executor CHECK (executor IN ('python','go'));

CREATE INDEX ix_jobs_claim ON jobs (executor, status, priority DESC, next_run_at) WHERE status = 'pending';

-- Running upgrade 4a5162738495 -> 7a087faeed5b

CREATE TABLE workspaces (
    id VARCHAR(64) NOT NULL,
    name TEXT NOT NULL,
    slug TEXT NOT NULL,
    description TEXT DEFAULT '' NOT NULL,
    icon TEXT DEFAULT '🏢' NOT NULL,
    owner_id INTEGER,
    created_at FLOAT(53),
    updated_at FLOAT(53),
    PRIMARY KEY (id),
    UNIQUE (slug)
);

CREATE TABLE workspace_members (
    workspace_id VARCHAR(64) NOT NULL,
    user_id INTEGER NOT NULL,
    role TEXT DEFAULT 'member' NOT NULL,
    created_at FLOAT(53),
    PRIMARY KEY (workspace_id, user_id),
    FOREIGN KEY(workspace_id) REFERENCES workspaces (id)
);

CREATE TABLE user_active_workspace (
    user_id SERIAL NOT NULL,
    workspace_id VARCHAR(64) NOT NULL,
    PRIMARY KEY (user_id)
);

CREATE TABLE workspace_oidc_identities (
    workspace_id VARCHAR(64) NOT NULL,
    issuer TEXT NOT NULL,
    subject TEXT NOT NULL,
    user_id INTEGER NOT NULL,
    email TEXT DEFAULT '' NOT NULL,
    created_at FLOAT(53) NOT NULL,
    PRIMARY KEY (workspace_id, issuer, subject),
    UNIQUE (workspace_id, user_id),
    FOREIGN KEY(workspace_id, user_id) REFERENCES workspace_members (workspace_id, user_id)
);

CREATE TABLE workspace_scim_tokens (
    workspace_id VARCHAR(64) NOT NULL,
    token_hash TEXT NOT NULL,
    token_prefix TEXT NOT NULL,
    created_at FLOAT(53) NOT NULL,
    expires_at FLOAT(53),
    last_used_at FLOAT(53),
    created_by INTEGER,
    PRIMARY KEY (workspace_id),
    FOREIGN KEY(workspace_id) REFERENCES workspaces (id)
);

CREATE TABLE workspace_settings (
    workspace_id VARCHAR(64) NOT NULL,
    key TEXT NOT NULL,
    value TEXT DEFAULT '' NOT NULL,
    PRIMARY KEY (workspace_id, key),
    FOREIGN KEY(workspace_id) REFERENCES workspaces (id)
);

CREATE TABLE workspace_member_permissions (
    workspace_id VARCHAR(64) NOT NULL,
    user_id INTEGER NOT NULL,
    permission TEXT NOT NULL,
    effect TEXT NOT NULL,
    updated_at FLOAT(53) NOT NULL,
    PRIMARY KEY (workspace_id, user_id, permission),
    CONSTRAINT ck_wmp_effect CHECK (effect IN ('allow', 'deny')),
    FOREIGN KEY(workspace_id, user_id) REFERENCES workspace_members (workspace_id, user_id)
);

CREATE TABLE workspace_scim_users (
    workspace_id VARCHAR(64) NOT NULL,
    user_id INTEGER NOT NULL,
    external_id TEXT DEFAULT '' NOT NULL,
    display_name TEXT DEFAULT '' NOT NULL,
    active BOOLEAN DEFAULT true NOT NULL,
    created_at FLOAT(53) NOT NULL,
    updated_at FLOAT(53) NOT NULL,
    PRIMARY KEY (workspace_id, user_id),
    FOREIGN KEY(workspace_id) REFERENCES workspaces (id)
);

CREATE TABLE workspace_scim_groups (
    id VARCHAR(64) NOT NULL,
    workspace_id VARCHAR(64) NOT NULL,
    external_id TEXT DEFAULT '' NOT NULL,
    display_name TEXT NOT NULL,
    created_at FLOAT(53) NOT NULL,
    updated_at FLOAT(53) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE (workspace_id, display_name),
    FOREIGN KEY(workspace_id) REFERENCES workspaces (id)
);

CREATE TABLE workspace_scim_group_members (
    workspace_id VARCHAR(64) NOT NULL,
    group_id VARCHAR(64) NOT NULL,
    user_id INTEGER NOT NULL,
    created_at FLOAT(53) NOT NULL,
    PRIMARY KEY (workspace_id, group_id, user_id),
    FOREIGN KEY(group_id) REFERENCES workspace_scim_groups (id) ON DELETE CASCADE
);

CREATE TABLE app_settings (
    key TEXT NOT NULL,
    value TEXT DEFAULT '' NOT NULL,
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL,
    PRIMARY KEY (key)
);

GRANT SELECT, INSERT, UPDATE, DELETE ON workspaces TO {{APP_ROLE}};

GRANT SELECT, INSERT, UPDATE, DELETE ON workspace_members TO {{APP_ROLE}};

GRANT SELECT, INSERT, UPDATE, DELETE ON user_active_workspace TO {{APP_ROLE}};

GRANT SELECT, INSERT, UPDATE, DELETE ON workspace_oidc_identities TO {{APP_ROLE}};

GRANT SELECT, INSERT, UPDATE, DELETE ON workspace_scim_tokens TO {{APP_ROLE}};

GRANT SELECT, INSERT, UPDATE, DELETE ON workspace_settings TO {{APP_ROLE}};

GRANT SELECT, INSERT, UPDATE, DELETE ON workspace_member_permissions TO {{APP_ROLE}};

GRANT SELECT, INSERT, UPDATE, DELETE ON workspace_scim_users TO {{APP_ROLE}};

GRANT SELECT, INSERT, UPDATE, DELETE ON workspace_scim_groups TO {{APP_ROLE}};

GRANT SELECT, INSERT, UPDATE, DELETE ON workspace_scim_group_members TO {{APP_ROLE}};

GRANT SELECT, INSERT, UPDATE, DELETE ON app_settings TO {{APP_ROLE}};

ALTER TABLE workspace_settings ENABLE ROW LEVEL SECURITY;

ALTER TABLE workspace_settings FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS workspace_isolation ON workspace_settings;

CREATE POLICY workspace_isolation ON workspace_settings
            USING ((workspace_id)::text = current_setting('app.workspace_id', true))
            WITH CHECK ((workspace_id)::text = current_setting('app.workspace_id', true));

ALTER TABLE workspace_member_permissions ENABLE ROW LEVEL SECURITY;

ALTER TABLE workspace_member_permissions FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS workspace_isolation ON workspace_member_permissions;

CREATE POLICY workspace_isolation ON workspace_member_permissions
            USING ((workspace_id)::text = current_setting('app.workspace_id', true))
            WITH CHECK ((workspace_id)::text = current_setting('app.workspace_id', true));

ALTER TABLE workspace_scim_users ENABLE ROW LEVEL SECURITY;

ALTER TABLE workspace_scim_users FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS workspace_isolation ON workspace_scim_users;

CREATE POLICY workspace_isolation ON workspace_scim_users
            USING ((workspace_id)::text = current_setting('app.workspace_id', true))
            WITH CHECK ((workspace_id)::text = current_setting('app.workspace_id', true));

ALTER TABLE workspace_scim_groups ENABLE ROW LEVEL SECURITY;

ALTER TABLE workspace_scim_groups FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS workspace_isolation ON workspace_scim_groups;

CREATE POLICY workspace_isolation ON workspace_scim_groups
            USING ((workspace_id)::text = current_setting('app.workspace_id', true))
            WITH CHECK ((workspace_id)::text = current_setting('app.workspace_id', true));

ALTER TABLE workspace_scim_group_members ENABLE ROW LEVEL SECURITY;

ALTER TABLE workspace_scim_group_members FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS workspace_isolation ON workspace_scim_group_members;

CREATE POLICY workspace_isolation ON workspace_scim_group_members
            USING ((workspace_id)::text = current_setting('app.workspace_id', true))
            WITH CHECK ((workspace_id)::text = current_setting('app.workspace_id', true));

-- Running upgrade 7a087faeed5b -> e1f5a9b3d7c2

CREATE TABLE chat_conversations (
    id TEXT NOT NULL,
    title TEXT DEFAULT 'New Chat' NOT NULL,
    workspace_id VARCHAR(64) NOT NULL,
    user_id TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL,
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL,
    PRIMARY KEY (id)
);

CREATE INDEX idx_chat_conv_tenant ON chat_conversations (workspace_id, user_id, updated_at);

CREATE TABLE chat_messages (
    id TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    workspace_id VARCHAR(64) NOT NULL,
    role TEXT NOT NULL,
    content TEXT DEFAULT '' NOT NULL,
    tool_data TEXT,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT ck_chat_messages_role CHECK (role IN ('user', 'assistant', 'system', 'tool')),
    FOREIGN KEY(conversation_id) REFERENCES chat_conversations (id) ON DELETE CASCADE
);

CREATE INDEX idx_chat_messages_conv ON chat_messages (conversation_id, created_at);

CREATE TABLE chat_tool_approvals (
    id TEXT NOT NULL,
    workspace_id VARCHAR(64) NOT NULL,
    user_id TEXT NOT NULL,
    tool_call_json TEXT NOT NULL,
    status TEXT DEFAULT 'pending' NOT NULL,
    created_at_epoch BIGINT NOT NULL,
    PRIMARY KEY (id)
);

CREATE INDEX idx_chat_tool_approvals_tenant ON chat_tool_approvals (workspace_id, user_id, created_at_epoch);

CREATE TABLE workbook_functions (
    id TEXT NOT NULL,
    name TEXT NOT NULL,
    description TEXT DEFAULT '',
    category TEXT DEFAULT 'custom',
    columns_chain JSONB DEFAULT '[]'::jsonb NOT NULL,
    version INTEGER DEFAULT '1',
    usage_count INTEGER DEFAULT '0',
    created_at FLOAT(53),
    updated_at FLOAT(53),
    PRIMARY KEY (id)
);

CREATE TABLE chat_memories (
    id BIGSERIAL NOT NULL,
    user_id TEXT NOT NULL,
    text TEXT NOT NULL,
    metadata JSONB DEFAULT '{}'::jsonb NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL,
    PRIMARY KEY (id)
);

CREATE INDEX ix_chat_memories_user ON chat_memories (user_id);

GRANT SELECT, INSERT, UPDATE, DELETE ON chat_conversations TO {{APP_ROLE}};

GRANT SELECT, INSERT, UPDATE, DELETE ON chat_messages TO {{APP_ROLE}};

GRANT SELECT, INSERT, UPDATE, DELETE ON chat_tool_approvals TO {{APP_ROLE}};

GRANT SELECT, INSERT, UPDATE, DELETE ON workbook_functions TO {{APP_ROLE}};

GRANT SELECT, INSERT, UPDATE, DELETE ON chat_memories TO {{APP_ROLE}};

GRANT USAGE, SELECT ON SEQUENCE chat_memories_id_seq TO {{APP_ROLE}};

ALTER TABLE chat_conversations ENABLE ROW LEVEL SECURITY;

ALTER TABLE chat_conversations FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS workspace_isolation ON chat_conversations;

CREATE POLICY workspace_isolation ON chat_conversations
            USING ((workspace_id)::text = current_setting('app.workspace_id', true))
            WITH CHECK ((workspace_id)::text = current_setting('app.workspace_id', true));

ALTER TABLE chat_messages ENABLE ROW LEVEL SECURITY;

ALTER TABLE chat_messages FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS workspace_isolation ON chat_messages;

CREATE POLICY workspace_isolation ON chat_messages
            USING ((workspace_id)::text = current_setting('app.workspace_id', true))
            WITH CHECK ((workspace_id)::text = current_setting('app.workspace_id', true));

ALTER TABLE chat_tool_approvals ENABLE ROW LEVEL SECURITY;

ALTER TABLE chat_tool_approvals FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS workspace_isolation ON chat_tool_approvals;

CREATE POLICY workspace_isolation ON chat_tool_approvals
            USING ((workspace_id)::text = current_setting('app.workspace_id', true))
            WITH CHECK ((workspace_id)::text = current_setting('app.workspace_id', true));

-- Running upgrade e1f5a9b3d7c2 -> f2a6b0c4d8e3

CREATE TABLE enrichment_cache (
    workspace_id TEXT NOT NULL,
    cache_key TEXT NOT NULL,
    field_name TEXT NOT NULL,
    value JSONB,
    provider TEXT,
    confidence FLOAT(53),
    cached_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    expires_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    PRIMARY KEY (workspace_id, cache_key, field_name)
);

CREATE TABLE email_verify_cache (
    workspace_id TEXT NOT NULL,
    email_hash TEXT NOT NULL,
    result JSONB NOT NULL,
    cached_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    expires_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
    PRIMARY KEY (workspace_id, email_hash)
);

CREATE INDEX ix_enrichment_cache_expires ON enrichment_cache (expires_at);

CREATE INDEX ix_email_verify_cache_expires ON email_verify_cache (expires_at);

GRANT SELECT, INSERT, UPDATE, DELETE ON enrichment_cache TO {{APP_ROLE}};

ALTER TABLE enrichment_cache ENABLE ROW LEVEL SECURITY;

ALTER TABLE enrichment_cache FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS workspace_isolation ON enrichment_cache;

CREATE POLICY workspace_isolation ON enrichment_cache
            USING ((workspace_id)::text = current_setting('app.workspace_id', true))
            WITH CHECK ((workspace_id)::text = current_setting('app.workspace_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON email_verify_cache TO {{APP_ROLE}};

ALTER TABLE email_verify_cache ENABLE ROW LEVEL SECURITY;

ALTER TABLE email_verify_cache FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS workspace_isolation ON email_verify_cache;

CREATE POLICY workspace_isolation ON email_verify_cache
            USING ((workspace_id)::text = current_setting('app.workspace_id', true))
            WITH CHECK ((workspace_id)::text = current_setting('app.workspace_id', true));

-- Running upgrade f2a6b0c4d8e3 -> a7c1e9d3b5f2

CREATE OR REPLACE FUNCTION public.outreach_send_by_message_id(p_message_id text)
        RETURNS TABLE (workspace_id text, send_id integer, sequence_id text,
                       enrollment_id integer, to_email text)
        LANGUAGE sql STABLE SECURITY DEFINER
        SET search_path = pg_catalog, public, pg_temp
        AS $$
            SELECT s.workspace_id::text, s.id, s.sequence_id::text,
                   s.enrollment_id, s.to_email::text
            FROM public.outreach_sends s
            WHERE s.message_id = p_message_id
            ORDER BY s.id DESC
            LIMIT 1
        $$;

REVOKE ALL ON FUNCTION public.outreach_send_by_message_id(text) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION public.outreach_send_by_message_id(text) TO {{APP_ROLE}};

-- Running upgrade a7c1e9d3b5f2 -> c9d2e4f6a8b1

CREATE TABLE collection_jobs (
    id TEXT NOT NULL,
    workspace_id TEXT NOT NULL,
    query TEXT NOT NULL,
    intent TEXT DEFAULT 'market_search' NOT NULL,
    intent_details TEXT DEFAULT '{}' NOT NULL,
    status TEXT DEFAULT 'pending' NOT NULL,
    tier INTEGER DEFAULT '1' NOT NULL,
    attempts INTEGER DEFAULT '0' NOT NULL,
    max_attempts INTEGER DEFAULT '3' NOT NULL,
    leads_found INTEGER DEFAULT '0' NOT NULL,
    proxy_used TEXT DEFAULT '' NOT NULL,
    error TEXT DEFAULT '' NOT NULL,
    created_at TEXT DEFAULT '' NOT NULL,
    started_at TEXT DEFAULT '' NOT NULL,
    completed_at TEXT DEFAULT '' NOT NULL,
    PRIMARY KEY (id)
);

CREATE TABLE collection_job_stages (
    id BIGSERIAL NOT NULL,
    workspace_id TEXT NOT NULL,
    job_id TEXT NOT NULL,
    stage TEXT NOT NULL,
    status TEXT DEFAULT 'running' NOT NULL,
    input_count INTEGER DEFAULT '0' NOT NULL,
    output_count INTEGER DEFAULT '0' NOT NULL,
    rejected_count INTEGER DEFAULT '0' NOT NULL,
    details TEXT DEFAULT '{}' NOT NULL,
    started_at TEXT DEFAULT '' NOT NULL,
    completed_at TEXT DEFAULT '' NOT NULL,
    PRIMARY KEY (id),
    FOREIGN KEY(job_id) REFERENCES collection_jobs (id) ON DELETE CASCADE
);

CREATE INDEX ix_collection_jobs_ws_status ON collection_jobs (workspace_id, status);

CREATE INDEX ix_collection_jobs_ws_created ON collection_jobs (workspace_id, created_at DESC);

CREATE INDEX ix_collection_job_stages_job ON collection_job_stages (job_id);

GRANT USAGE, SELECT ON SEQUENCE collection_job_stages_id_seq TO {{APP_ROLE}};

GRANT SELECT, INSERT, UPDATE, DELETE ON collection_jobs TO {{APP_ROLE}};

ALTER TABLE collection_jobs ENABLE ROW LEVEL SECURITY;

ALTER TABLE collection_jobs FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS workspace_isolation ON collection_jobs;

CREATE POLICY workspace_isolation ON collection_jobs
            USING ((workspace_id)::text = current_setting('app.workspace_id', true))
            WITH CHECK ((workspace_id)::text = current_setting('app.workspace_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON collection_job_stages TO {{APP_ROLE}};

ALTER TABLE collection_job_stages ENABLE ROW LEVEL SECURITY;

ALTER TABLE collection_job_stages FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS workspace_isolation ON collection_job_stages;

CREATE POLICY workspace_isolation ON collection_job_stages
            USING ((workspace_id)::text = current_setting('app.workspace_id', true))
            WITH CHECK ((workspace_id)::text = current_setting('app.workspace_id', true));

-- Running upgrade c9d2e4f6a8b1 -> d3f5a7b9c1e2

ALTER TABLE destination_runs ALTER COLUMN status TYPE VARCHAR(32);

-- Running upgrade d3f5a7b9c1e2 -> e7b2c4d6f8a0

REVOKE ALL ON FUNCTION public.outreach_send_by_message_id(text) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION public.outreach_send_by_message_id(text) TO {{APP_ROLE}};

CREATE INDEX IF NOT EXISTS ix_outreach_sends_message_id ON outreach_sends (message_id);
