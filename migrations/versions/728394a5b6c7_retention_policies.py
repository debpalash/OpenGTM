"""Add enforceable workspace retention policies and durable runs.

Revision ID: 728394a5b6c7
Revises: 61728394a5b6
"""
import os
import re
from alembic import op
import sqlalchemy as sa

revision = "728394a5b6c7"
down_revision = "61728394a5b6"
branch_labels = None
depends_on = None
APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")


def upgrade():
    op.create_table("retention_policies", sa.Column("workspace_id", sa.String(64), primary_key=True), sa.Column("enabled", sa.Boolean(), nullable=False, server_default=sa.false()), sa.Column("legal_hold", sa.Boolean(), nullable=False, server_default=sa.false()), sa.Column("retention_days", sa.JSON(), nullable=False), sa.Column("next_run_at", sa.DateTime()), sa.Column("updated_by", sa.Integer()), sa.Column("created_at", sa.DateTime(), nullable=False, server_default=sa.func.now()), sa.Column("updated_at", sa.DateTime(), nullable=False, server_default=sa.func.now()))
    op.create_table("retention_schedules", sa.Column("workspace_id", sa.String(64), primary_key=True), sa.Column("enabled", sa.Boolean(), nullable=False, server_default=sa.false()), sa.Column("next_run_at", sa.DateTime()), sa.Column("updated_at", sa.DateTime(), nullable=False, server_default=sa.func.now()))
    op.create_table("retention_runs", sa.Column("id", sa.String(36), primary_key=True), sa.Column("workspace_id", sa.String(64), nullable=False), sa.Column("status", sa.String(24), nullable=False), sa.Column("requested_by", sa.String(64)), sa.Column("policy_snapshot", sa.JSON(), nullable=False), sa.Column("deleted_counts", sa.JSON(), nullable=False), sa.Column("error", sa.Text()), sa.Column("started_at", sa.DateTime()), sa.Column("finished_at", sa.DateTime()), sa.Column("created_at", sa.DateTime(), nullable=False, server_default=sa.func.now()))
    op.create_index("ix_retention_runs_workspace_id", "retention_runs", ["workspace_id"])
    op.create_index("ix_retention_runs_ws_created", "retention_runs", ["workspace_id", "created_at"])
    if op.get_bind().dialect.name == "postgresql":
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", APP_DB_ROLE): raise RuntimeError("unsafe database role")
        for table in ("retention_policies", "retention_runs"):
            op.execute(sa.text(f'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE "{table}" TO "{APP_DB_ROLE}"'))
            op.execute(sa.text(f'ALTER TABLE "{table}" ENABLE ROW LEVEL SECURITY'))
            op.execute(sa.text(f'ALTER TABLE "{table}" FORCE ROW LEVEL SECURITY'))
            op.execute(sa.text(f'CREATE POLICY workspace_isolation ON "{table}" USING (workspace_id = current_setting(\'app.workspace_id\', true)) WITH CHECK (workspace_id = current_setting(\'app.workspace_id\', true))'))
        op.execute(sa.text(f'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE "retention_schedules" TO "{APP_DB_ROLE}"'))


def downgrade():
    op.drop_table("retention_runs")
    op.drop_table("retention_schedules")
    op.drop_table("retention_policies")
