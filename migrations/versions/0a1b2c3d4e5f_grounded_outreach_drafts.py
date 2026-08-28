"""Grounded outreach drafts.

Revision ID: 0a1b2c3d4e5f
Revises: f9a0b1c2d3e4
"""

from typing import Sequence, Union
import os
import re

from alembic import op
import sqlalchemy as sa


revision: str = "0a1b2c3d4e5f"
down_revision: Union[str, Sequence[str], None] = "f9a0b1c2d3e4"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None
APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")


def _app_db_role() -> str:
    if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", APP_DB_ROLE):
        raise RuntimeError("YUPCHA_APP_DB_ROLE is not a safe SQL identifier")
    return APP_DB_ROLE


def upgrade() -> None:
    op.create_table(
        "outreach_drafts",
        sa.Column("id", sa.String(length=36), nullable=False),
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("action_idempotency_key", sa.String(length=255), nullable=False),
        sa.Column("conversation_id", sa.String(length=64), nullable=False),
        sa.Column("source_action_id", sa.String(length=255), nullable=False),
        sa.Column("person_id", sa.String(length=80), nullable=False),
        sa.Column("person_name", sa.String(length=200), nullable=False),
        sa.Column("company", sa.String(length=200), nullable=False),
        sa.Column("title", sa.String(length=240), nullable=True),
        sa.Column("to_email", sa.String(length=320), nullable=False),
        sa.Column("contact_status", sa.String(length=20), nullable=False),
        sa.Column("risky_approved", sa.Boolean(), server_default=sa.false(), nullable=False),
        sa.Column("generic_inbox", sa.Boolean(), server_default=sa.false(), nullable=False),
        sa.Column("is_role_address", sa.Boolean(), server_default=sa.false(), nullable=False),
        sa.Column("subject", sa.String(length=300), nullable=False),
        sa.Column("body_text", sa.Text(), nullable=False),
        sa.Column("sentence_evidence", sa.JSON(), nullable=False),
        sa.Column("source_snapshot", sa.JSON(), nullable=False),
        sa.Column("state", sa.String(length=20), server_default="draft", nullable=False),
        sa.Column("sent_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("created_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.Column("updated_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.PrimaryKeyConstraint("id"),
        sa.UniqueConstraint(
            "workspace_id", "action_idempotency_key", name="uq_outreach_draft_ws_action"
        ),
    )
    op.create_index(
        "ix_outreach_draft_ws_created", "outreach_drafts", ["workspace_id", "created_at"]
    )
    op.create_index(
        "ix_outreach_draft_ws_person", "outreach_drafts", ["workspace_id", "person_id"]
    )
    if op.get_bind().dialect.name == "postgresql":
        role = _app_db_role()
        op.execute(
            f'GRANT SELECT, INSERT, UPDATE, DELETE ON outreach_drafts TO "{role}"'
        )
        op.execute("ALTER TABLE outreach_drafts ENABLE ROW LEVEL SECURITY")
        op.execute("ALTER TABLE outreach_drafts FORCE ROW LEVEL SECURITY")
        op.execute(
            """
            CREATE POLICY outreach_drafts_workspace_isolation ON outreach_drafts
            USING (workspace_id = current_setting('app.workspace_id', true))
            WITH CHECK (workspace_id = current_setting('app.workspace_id', true))
            """
        )


def downgrade() -> None:
    if op.get_bind().dialect.name == "postgresql":
        op.execute(
            "DROP POLICY IF EXISTS outreach_drafts_workspace_isolation ON outreach_drafts"
        )
    op.drop_index("ix_outreach_draft_ws_person", table_name="outreach_drafts")
    op.drop_index("ix_outreach_draft_ws_created", table_name="outreach_drafts")
    op.drop_table("outreach_drafts")
