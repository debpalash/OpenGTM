"""Add per-workspace plugin secrets.

``plugin_secrets`` holds the secrets a plugin declares in
``capabilities.secrets``, one row per (workspace, name). The value is stored
only as ciphertext in the same ``enc:v1:`` Fernet envelope the Python app uses
for workspace integration secrets (``apps/api/services/workspace/secrets.py``),
so both stacks can decrypt it with the shared ``SECRETS_MASTER_KEY``. The check
constraint makes it impossible to store anything that is not in that envelope,
so a bug cannot persist a plaintext value. The table is tenant data and uses
the canonical fail-closed workspace policy, forced for the table owner too.
It depends on PostgreSQL (the Go roles require it), so it is not created on
SQLite.

Revision ID: 9d2f4b6a8c10
Revises: 7c1e5a9d3b20
"""
import os

from alembic import op

revision = "9d2f4b6a8c10"
down_revision = "7c1e5a9d3b20"
branch_labels = None
depends_on = None


def upgrade():
    bind = op.get_bind()
    if bind.dialect.name != "postgresql":
        return
    role = bind.dialect.identifier_preparer.quote(os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app"))
    op.execute("""
        CREATE TABLE plugin_secrets (
            workspace_id TEXT NOT NULL,
            name TEXT NOT NULL,
            ciphertext TEXT NOT NULL,
            version INTEGER NOT NULL DEFAULT 1,
            created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
            created_by TEXT,
            updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
            updated_by TEXT,
            PRIMARY KEY (workspace_id, name),
            CONSTRAINT ck_plugin_secrets_name CHECK (name ~ '^[A-Za-z][A-Za-z0-9_]{0,127}$'),
            CONSTRAINT ck_plugin_secrets_envelope CHECK (ciphertext LIKE 'enc:v1:%')
        )""")
    op.execute(f"GRANT SELECT, INSERT, UPDATE, DELETE ON plugin_secrets TO {role}")
    op.execute("ALTER TABLE plugin_secrets ENABLE ROW LEVEL SECURITY")
    op.execute("ALTER TABLE plugin_secrets FORCE ROW LEVEL SECURITY")
    op.execute("""CREATE POLICY workspace_isolation ON plugin_secrets
        USING (workspace_id = current_setting('app.workspace_id', true))
        WITH CHECK (workspace_id = current_setting('app.workspace_id', true))""")


def downgrade():
    if op.get_bind().dialect.name == "postgresql":
        op.execute("DROP TABLE IF EXISTS plugin_secrets")
