"""M8 multi-host: the collection job ledger in PostgreSQL.

The lead-collection pipeline keeps its bookkeeping (``jobs``, ``job_stages``) in
a per-workspace SQLite file (``data/leads.db``, ``data/workspaces/<slug>/
leads.db``), even when the leads themselves live in the shared RLS tables. A
collect job claimed by a worker on host B is then invisible to the API on host
A. These tables hold the same ledger in PostgreSQL, selected by
``COLLECTION_LEDGER_STORE=postgres``. They are not the durable queue
(``jobs``), which is unchanged.

Time columns are TEXT (ISO-8601) exactly as in the SQLite ledger, so the API
returns identical values. ``collection_job_stages.legacy_id`` carries the
SQLite autoincrement id of a backfilled stage so a backfill is idempotent;
natively created stages leave it NULL.

PostgreSQL only. Both tables are tenant data: FORCE ROW LEVEL SECURITY with the
canonical fail-closed workspace policy. LLM usage needs no table here: it is
already ``llm_usage_daily``.

Additive and reversible.

Revision ID: b5d7f9a1c3e6
Revises: 9b3d5f7a2c41
"""
import os

from alembic import op

revision = "b5d7f9a1c3e6"
down_revision = "9b3d5f7a2c41"
branch_labels = None
depends_on = None

TABLES = ("collection_jobs", "collection_job_stages")


def upgrade():
    bind = op.get_bind()
    if bind.dialect.name != "postgresql":
        return
    role = bind.dialect.identifier_preparer.quote(os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app"))

    op.execute("""
        CREATE TABLE collection_jobs (
            id TEXT PRIMARY KEY,
            workspace_id TEXT NOT NULL,
            query TEXT NOT NULL,
            intent TEXT NOT NULL DEFAULT 'market_search',
            intent_details TEXT NOT NULL DEFAULT '{}',
            status TEXT NOT NULL DEFAULT 'pending',
            tier INTEGER NOT NULL DEFAULT 1,
            attempts INTEGER NOT NULL DEFAULT 0,
            max_attempts INTEGER NOT NULL DEFAULT 3,
            leads_found INTEGER NOT NULL DEFAULT 0,
            proxy_used TEXT NOT NULL DEFAULT '',
            error TEXT NOT NULL DEFAULT '',
            created_at TEXT NOT NULL DEFAULT '',
            started_at TEXT NOT NULL DEFAULT '',
            completed_at TEXT NOT NULL DEFAULT ''
        )""")
    op.execute("CREATE INDEX ix_collection_jobs_ws_status ON collection_jobs (workspace_id, status, created_at)")
    op.execute("""
        CREATE TABLE collection_job_stages (
            id BIGSERIAL PRIMARY KEY,
            workspace_id TEXT NOT NULL,
            job_id TEXT NOT NULL,
            stage TEXT NOT NULL,
            status TEXT NOT NULL DEFAULT 'running',
            input_count INTEGER NOT NULL DEFAULT 0,
            output_count INTEGER NOT NULL DEFAULT 0,
            rejected_count INTEGER NOT NULL DEFAULT 0,
            details TEXT NOT NULL DEFAULT '{}',
            started_at TEXT NOT NULL DEFAULT '',
            completed_at TEXT NOT NULL DEFAULT '',
            legacy_id BIGINT
        )""")
    op.execute("CREATE INDEX ix_collection_job_stages_job ON collection_job_stages (workspace_id, job_id, id)")
    op.execute("CREATE UNIQUE INDEX uq_collection_job_stages_legacy "
               "ON collection_job_stages (workspace_id, job_id, legacy_id) WHERE legacy_id IS NOT NULL")

    op.execute(f"GRANT USAGE, SELECT ON SEQUENCE collection_job_stages_id_seq TO {role}")
    for table in TABLES:
        op.execute(f"GRANT SELECT, INSERT, UPDATE, DELETE ON {table} TO {role}")
        op.execute(f"ALTER TABLE {table} ENABLE ROW LEVEL SECURITY")
        op.execute(f"ALTER TABLE {table} FORCE ROW LEVEL SECURITY")
        op.execute(f"""CREATE POLICY workspace_isolation ON {table}
            USING (workspace_id = current_setting('app.workspace_id', true))
            WITH CHECK (workspace_id = current_setting('app.workspace_id', true))""")


def downgrade():
    if op.get_bind().dialect.name != "postgresql":
        return
    for table in reversed(TABLES):
        op.execute(f"DROP TABLE IF EXISTS {table}")
