"""Route job types to one executor and add plugin run storage.

``job_executor_routes`` makes queue ownership explicit while Python and Go
workers share the ``jobs`` table: a type with no row (or an explicit
``python`` row) is claimed only by Python, a ``go`` row only by Go. Changing a
route is an operator action (``opengtm routes set``) taken after draining
in-flight attempts, so it is never derived from configuration.

``plugin_runs``/``plugin_results`` hold the first Go-only job type. They are
tenant data and use the canonical fail-closed workspace policy. They depend on
JSONB and ``gen_random_uuid()``, and the Go roles require PostgreSQL, so they
are not created on SQLite.

Revision ID: 7c1e5a9d3b20
Revises: 3f4051627384
"""
import os

from alembic import op
import sqlalchemy as sa

revision = "7c1e5a9d3b20"
down_revision = "3f4051627384"
branch_labels = None
depends_on = None

PLUGIN_TABLES = ("plugin_runs", "plugin_results")


def upgrade():
    op.create_table(
        "job_executor_routes",
        sa.Column("job_type", sa.String(), primary_key=True),
        sa.Column("executor", sa.String(), nullable=False),
        sa.Column("updated_at", sa.DateTime(timezone=True), nullable=False,
                  server_default=sa.func.now()),
        sa.CheckConstraint("executor IN ('python','go')", name="ck_job_executor_routes_executor"),
    )
    op.execute("INSERT INTO job_executor_routes (job_type, executor) VALUES ('plugin_run', 'go')")

    bind = op.get_bind()
    if bind.dialect.name != "postgresql":
        return

    role = bind.dialect.identifier_preparer.quote(os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app"))
    op.execute(f"GRANT SELECT, INSERT, UPDATE, DELETE ON job_executor_routes TO {role}")

    op.execute("""
        CREATE TABLE plugin_runs (
            id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
            workspace_id TEXT NOT NULL,
            plugin_name TEXT NOT NULL,
            plugin_version TEXT,
            inputs JSONB NOT NULL,
            status TEXT NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending','running','completed','failed','cancelled')),
            job_id INTEGER,
            created_by TEXT,
            created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
            started_at TIMESTAMPTZ,
            completed_at TIMESTAMPTZ,
            error TEXT,
            stats JSONB NOT NULL DEFAULT '{}'
        )""")
    op.execute("CREATE INDEX ix_plugin_runs_workspace_created ON plugin_runs (workspace_id, created_at DESC)")
    op.execute("""
        CREATE TABLE plugin_results (
            id BIGSERIAL PRIMARY KEY,
            run_id UUID NOT NULL REFERENCES plugin_runs(id) ON DELETE CASCADE,
            workspace_id TEXT NOT NULL,
            record_index INTEGER NOT NULL,
            data JSONB NOT NULL,
            evidence JSONB NOT NULL,
            created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
            UNIQUE (run_id, record_index)
        )""")
    op.execute(f"GRANT USAGE, SELECT ON SEQUENCE plugin_results_id_seq TO {role}")
    for table in PLUGIN_TABLES:
        op.execute(f"GRANT SELECT, INSERT, UPDATE, DELETE ON {table} TO {role}")
        op.execute(f"ALTER TABLE {table} ENABLE ROW LEVEL SECURITY")
        op.execute(f"ALTER TABLE {table} FORCE ROW LEVEL SECURITY")
        op.execute(f"""CREATE POLICY workspace_isolation ON {table}
            USING (workspace_id = current_setting('app.workspace_id', true))
            WITH CHECK (workspace_id = current_setting('app.workspace_id', true))""")


def downgrade():
    if op.get_bind().dialect.name == "postgresql":
        op.execute("DROP TABLE IF EXISTS plugin_results")
        op.execute("DROP TABLE IF EXISTS plugin_runs")
    op.drop_table("job_executor_routes")
