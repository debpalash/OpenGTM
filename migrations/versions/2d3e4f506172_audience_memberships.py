"""Materialize audience membership and membership events.

Revision ID: 2d3e4f506172
Revises: 1c2d3e4f5061
"""

import os
import re
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa

revision: str = "2d3e4f506172"
down_revision: Union[str, Sequence[str], None] = "1c2d3e4f5061"
branch_labels = None
depends_on = None
APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")
TABLES = ("audience_members", "audience_membership_events")


def upgrade() -> None:
    op.add_column("audiences", sa.Column("refreshed_at", sa.DateTime(), nullable=True))
    op.create_table(
        "audience_members",
        sa.Column("id", sa.Integer(), autoincrement=True, nullable=False),
        sa.Column("workspace_id", sa.String(), nullable=False),
        sa.Column("audience_id", sa.String(), nullable=False),
        sa.Column("lead_id", sa.Integer(), nullable=False),
        sa.Column("snapshot", sa.JSON(), nullable=False),
        sa.Column("joined_at", sa.DateTime(), nullable=False, server_default=sa.func.now()),
        sa.Column("last_seen_at", sa.DateTime(), nullable=False, server_default=sa.func.now()),
        sa.ForeignKeyConstraint(["audience_id"], ["audiences.id"], ondelete="CASCADE"),
        sa.PrimaryKeyConstraint("id"),
        sa.UniqueConstraint("audience_id", "lead_id", name="uq_audience_members_audience_lead"),
    )
    op.create_index("ix_audience_members_workspace_id", "audience_members", ["workspace_id"])
    op.create_index("ix_audience_members_audience_id", "audience_members", ["audience_id"])
    op.create_index("ix_audience_members_lead_id", "audience_members", ["lead_id"])
    op.create_index("ix_audience_members_workspace_audience", "audience_members", ["workspace_id", "audience_id"])
    op.create_table(
        "audience_membership_events",
        sa.Column("id", sa.Integer(), autoincrement=True, nullable=False),
        sa.Column("workspace_id", sa.String(), nullable=False),
        sa.Column("audience_id", sa.String(), nullable=False),
        sa.Column("lead_id", sa.Integer(), nullable=False),
        sa.Column("event_type", sa.String(length=16), nullable=False),
        sa.Column("snapshot", sa.JSON(), nullable=False),
        sa.Column("created_at", sa.DateTime(), nullable=False, server_default=sa.func.now()),
        sa.ForeignKeyConstraint(["audience_id"], ["audiences.id"], ondelete="CASCADE"),
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_index("ix_audience_membership_events_workspace_id", "audience_membership_events", ["workspace_id"])
    op.create_index("ix_audience_membership_events_audience_id", "audience_membership_events", ["audience_id"])
    op.create_index("ix_audience_membership_events_lead_id", "audience_membership_events", ["lead_id"])
    op.create_index("ix_audience_events_workspace_audience_created", "audience_membership_events", ["workspace_id", "audience_id", "created_at"])

    if op.get_bind().dialect.name == "postgresql":
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", APP_DB_ROLE):
            raise RuntimeError("YUPCHA_APP_DB_ROLE is not a safe SQL identifier")
        for table in TABLES:
            op.execute(sa.text(f'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE "{table}" TO "{APP_DB_ROLE}"'))
            op.execute(sa.text(f'ALTER TABLE "{table}" ENABLE ROW LEVEL SECURITY'))
            op.execute(sa.text(f'ALTER TABLE "{table}" FORCE ROW LEVEL SECURITY'))
            op.execute(sa.text(
                f'CREATE POLICY workspace_isolation ON "{table}" '
                "USING (workspace_id = current_setting('app.workspace_id', true)) "
                "WITH CHECK (workspace_id = current_setting('app.workspace_id', true))"
            ))
        for sequence in ("audience_members_id_seq", "audience_membership_events_id_seq"):
            op.execute(sa.text(f'GRANT USAGE, SELECT ON SEQUENCE "{sequence}" TO "{APP_DB_ROLE}"'))


def downgrade() -> None:
    op.drop_index("ix_audience_events_workspace_audience_created", table_name="audience_membership_events")
    op.drop_index("ix_audience_membership_events_lead_id", table_name="audience_membership_events")
    op.drop_index("ix_audience_membership_events_audience_id", table_name="audience_membership_events")
    op.drop_index("ix_audience_membership_events_workspace_id", table_name="audience_membership_events")
    op.drop_table("audience_membership_events")
    op.drop_index("ix_audience_members_workspace_audience", table_name="audience_members")
    op.drop_index("ix_audience_members_lead_id", table_name="audience_members")
    op.drop_index("ix_audience_members_audience_id", table_name="audience_members")
    op.drop_index("ix_audience_members_workspace_id", table_name="audience_members")
    op.drop_table("audience_members")
    op.drop_column("audiences", "refreshed_at")
