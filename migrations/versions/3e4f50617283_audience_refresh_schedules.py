"""Add restart-safe audience refresh schedules.

Revision ID: 3e4f50617283
Revises: 2d3e4f506172
"""

import os
import re
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa

revision: str = "3e4f50617283"
down_revision: Union[str, Sequence[str], None] = "2d3e4f506172"
branch_labels = None
depends_on = None
APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")


def upgrade() -> None:
    op.add_column("audiences", sa.Column("refresh_enabled", sa.Boolean(), nullable=False, server_default=sa.true()))
    op.add_column("audiences", sa.Column("refresh_interval_minutes", sa.Integer(), nullable=False, server_default="60"))
    op.add_column("audiences", sa.Column("next_refresh_at", sa.DateTime(), nullable=True))
    op.create_table(
        "audience_schedules",
        sa.Column("audience_id", sa.String(), nullable=False),
        sa.Column("workspace_id", sa.String(), nullable=False),
        sa.Column("next_refresh_at", sa.DateTime(), nullable=True),
        sa.Column("enabled", sa.Boolean(), nullable=False, server_default=sa.true()),
        sa.Column("updated_at", sa.DateTime(), nullable=False, server_default=sa.func.now()),
        sa.PrimaryKeyConstraint("audience_id"),
    )
    op.create_index("ix_audience_schedules_due", "audience_schedules", ["enabled", "next_refresh_at"])
    if op.get_bind().dialect.name == "postgresql":
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", APP_DB_ROLE):
            raise RuntimeError("YUPCHA_APP_DB_ROLE is not a safe SQL identifier")
        op.execute(sa.text(
            f'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE "audience_schedules" TO "{APP_DB_ROLE}"'
        ))


def downgrade() -> None:
    op.drop_index("ix_audience_schedules_due", table_name="audience_schedules")
    op.drop_table("audience_schedules")
    op.drop_column("audiences", "next_refresh_at")
    op.drop_column("audiences", "refresh_interval_minutes")
    op.drop_column("audiences", "refresh_enabled")
