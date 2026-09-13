"""Add restart-safe recurring research playbooks.

Revision ID: 8394a5b6c7d8
Revises: 728394a5b6c7
"""
import os
import re

from alembic import op
import sqlalchemy as sa

revision = "8394a5b6c7d8"
down_revision = "728394a5b6c7"
branch_labels = None
depends_on = None
APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")


def upgrade():
    with op.batch_alter_table("research_playbooks") as batch:
        batch.add_column(sa.Column("schedule_audience_id", sa.String(), nullable=True))
        batch.add_column(sa.Column("schedule_interval_minutes", sa.Integer(), nullable=True))
        batch.add_column(sa.Column("next_run_at", sa.DateTime(), nullable=True))
        batch.create_foreign_key("fk_research_playbooks_schedule_audience", "audiences", ["schedule_audience_id"], ["id"], ondelete="SET NULL")
    op.create_table("playbook_schedules", sa.Column("playbook_id", sa.String(), primary_key=True), sa.Column("workspace_id", sa.String(), nullable=False), sa.Column("enabled", sa.Boolean(), nullable=False, server_default=sa.false()), sa.Column("next_run_at", sa.DateTime()), sa.Column("updated_at", sa.DateTime(), nullable=False, server_default=sa.func.now()))
    op.create_index("ix_playbook_schedules_workspace_id", "playbook_schedules", ["workspace_id"])
    if op.get_bind().dialect.name == "postgresql":
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", APP_DB_ROLE):
            raise RuntimeError("unsafe database role")
        op.execute(sa.text(f'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE "playbook_schedules" TO "{APP_DB_ROLE}"'))


def downgrade():
    op.drop_table("playbook_schedules")
    with op.batch_alter_table("research_playbooks") as batch:
        batch.drop_constraint("fk_research_playbooks_schedule_audience", type_="foreignkey")
        batch.drop_column("next_run_at")
        batch.drop_column("schedule_interval_minutes")
        batch.drop_column("schedule_audience_id")
