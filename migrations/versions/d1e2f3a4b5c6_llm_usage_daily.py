"""Add tenant-scoped daily LLM usage aggregates.

Revision ID: d1e2f3a4b5c6
Revises: c0d1e2f3a4b5
"""

import os
import re

from alembic import op
import sqlalchemy as sa


revision = "d1e2f3a4b5c6"
down_revision = "c0d1e2f3a4b5"
branch_labels = None
depends_on = None
APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")


def upgrade() -> None:
    op.create_table(
        "llm_usage_daily",
        sa.Column("id", sa.Integer(), primary_key=True, autoincrement=True),
        sa.Column("workspace_id", sa.String(), nullable=False),
        sa.Column("provider", sa.String(64), nullable=False),
        sa.Column("model", sa.String(255), nullable=False, server_default=""),
        sa.Column("date", sa.String(10), nullable=False),
        sa.Column("calls", sa.Integer(), nullable=False, server_default="0"),
        sa.Column("prompt_tokens", sa.Integer(), nullable=False, server_default="0"),
        sa.Column("completion_tokens", sa.Integer(), nullable=False, server_default="0"),
        sa.Column("total_tokens", sa.Integer(), nullable=False, server_default="0"),
        sa.Column("rate_limit", sa.Integer(), nullable=False, server_default="0"),
        sa.Column("rate_remaining", sa.Integer(), nullable=False, server_default="0"),
        sa.Column("rate_reset", sa.String(255), nullable=False, server_default=""),
        sa.Column("updated_at", sa.String(), nullable=False),
        sa.UniqueConstraint("workspace_id", "provider", "date", name="uq_llm_usage_ws_provider_date"),
    )
    op.create_index("ix_llm_usage_daily_workspace_id", "llm_usage_daily", ["workspace_id"])
    op.create_index("ix_llm_usage_ws_date", "llm_usage_daily", ["workspace_id", "date"])
    if op.get_bind().dialect.name == "postgresql":
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", APP_DB_ROLE):
            raise RuntimeError("unsafe database role")
        op.execute(sa.text(f'GRANT SELECT, INSERT, UPDATE ON TABLE "llm_usage_daily" TO "{APP_DB_ROLE}"'))
        op.execute(sa.text(f'GRANT USAGE, SELECT ON SEQUENCE "llm_usage_daily_id_seq" TO "{APP_DB_ROLE}"'))
        op.execute(sa.text('ALTER TABLE "llm_usage_daily" ENABLE ROW LEVEL SECURITY'))
        op.execute(sa.text('ALTER TABLE "llm_usage_daily" FORCE ROW LEVEL SECURITY'))
        op.execute(sa.text(
            "CREATE POLICY workspace_isolation ON llm_usage_daily "
            "USING (workspace_id = current_setting('app.workspace_id', true)) "
            "WITH CHECK (workspace_id = current_setting('app.workspace_id', true))"
        ))


def downgrade() -> None:
    op.drop_table("llm_usage_daily")
