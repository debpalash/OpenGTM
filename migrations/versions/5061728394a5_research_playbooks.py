"""Add reusable audience research playbooks and durable result ledgers.

Revision ID: 5061728394a5
Revises: 4f5061728394
"""
import os
import re
from alembic import op
import sqlalchemy as sa

revision = "5061728394a5"
down_revision = "4f5061728394"
branch_labels = None
depends_on = None
APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")
TABLES = ("research_playbooks", "playbook_runs", "playbook_results")


def upgrade():
    op.create_table("research_playbooks",
        sa.Column("id", sa.String(), primary_key=True), sa.Column("workspace_id", sa.String(), nullable=False),
        sa.Column("name", sa.String(200), nullable=False), sa.Column("description", sa.Text(), nullable=False),
        sa.Column("prompt_template", sa.Text(), nullable=False), sa.Column("output_format", sa.String(20), nullable=False),
        sa.Column("max_steps", sa.Integer(), nullable=False), sa.Column("cell_budget_usd", sa.Float(), nullable=False),
        sa.Column("version", sa.Integer(), nullable=False), sa.Column("enabled", sa.Boolean(), nullable=False, server_default=sa.true()),
        sa.Column("created_at", sa.DateTime(), nullable=False, server_default=sa.func.now()), sa.Column("updated_at", sa.DateTime(), nullable=False, server_default=sa.func.now()),
        sa.UniqueConstraint("workspace_id", "name", name="uq_research_playbook_name"))
    op.create_index("ix_research_playbooks_workspace_id", "research_playbooks", ["workspace_id"])
    op.create_table("playbook_runs",
        sa.Column("id", sa.String(), primary_key=True), sa.Column("workspace_id", sa.String(), nullable=False),
        sa.Column("playbook_id", sa.String(), nullable=False), sa.Column("audience_id", sa.String(), nullable=False),
        sa.Column("status", sa.String(24), nullable=False), sa.Column("prompt_version", sa.Integer(), nullable=False),
        sa.Column("prompt_snapshot", sa.Text(), nullable=False), sa.Column("max_members", sa.Integer(), nullable=False),
        sa.Column("attempted", sa.Integer(), nullable=False), sa.Column("succeeded", sa.Integer(), nullable=False), sa.Column("failed", sa.Integer(), nullable=False),
        sa.Column("error", sa.Text()), sa.Column("requested_by", sa.String()), sa.Column("started_at", sa.DateTime()), sa.Column("finished_at", sa.DateTime()),
        sa.Column("created_at", sa.DateTime(), nullable=False, server_default=sa.func.now()),
        sa.ForeignKeyConstraint(["playbook_id"], ["research_playbooks.id"], ondelete="CASCADE"), sa.ForeignKeyConstraint(["audience_id"], ["audiences.id"], ondelete="CASCADE"))
    for name, cols in (("ix_playbook_runs_workspace_id", ["workspace_id"]), ("ix_playbook_runs_playbook_id", ["playbook_id"]), ("ix_playbook_runs_audience_id", ["audience_id"]), ("ix_playbook_runs_ws_playbook_created", ["workspace_id", "playbook_id", "created_at"])):
        op.create_index(name, "playbook_runs", cols)
    op.create_table("playbook_results",
        sa.Column("id", sa.Integer(), primary_key=True, autoincrement=True), sa.Column("workspace_id", sa.String(), nullable=False),
        sa.Column("run_id", sa.String(), nullable=False), sa.Column("lead_id", sa.Integer(), nullable=False), sa.Column("status", sa.String(20), nullable=False),
        sa.Column("value", sa.Text(), nullable=False), sa.Column("result_metadata", sa.JSON(), nullable=False), sa.Column("error", sa.Text()), sa.Column("attempts", sa.Integer(), nullable=False),
        sa.Column("created_at", sa.DateTime(), nullable=False, server_default=sa.func.now()), sa.Column("updated_at", sa.DateTime(), nullable=False, server_default=sa.func.now()),
        sa.ForeignKeyConstraint(["run_id"], ["playbook_runs.id"], ondelete="CASCADE"), sa.UniqueConstraint("workspace_id", "run_id", "lead_id", name="uq_playbook_result_run_lead"))
    for name, cols in (("ix_playbook_results_workspace_id", ["workspace_id"]), ("ix_playbook_results_run_id", ["run_id"]), ("ix_playbook_results_lead_id", ["lead_id"])):
        op.create_index(name, "playbook_results", cols)
    if op.get_bind().dialect.name == "postgresql":
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", APP_DB_ROLE): raise RuntimeError("unsafe database role")
        for table in TABLES:
            op.execute(sa.text(f'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE "{table}" TO "{APP_DB_ROLE}"'))
            op.execute(sa.text(f'ALTER TABLE "{table}" ENABLE ROW LEVEL SECURITY'))
            op.execute(sa.text(f'ALTER TABLE "{table}" FORCE ROW LEVEL SECURITY'))
            op.execute(sa.text(f'CREATE POLICY workspace_isolation ON "{table}" USING (workspace_id = current_setting(\'app.workspace_id\', true)) WITH CHECK (workspace_id = current_setting(\'app.workspace_id\', true))'))
        op.execute(sa.text(f'GRANT USAGE, SELECT ON SEQUENCE "playbook_results_id_seq" TO "{APP_DB_ROLE}"'))


def downgrade():
    op.drop_table("playbook_results"); op.drop_table("playbook_runs"); op.drop_table("research_playbooks")
