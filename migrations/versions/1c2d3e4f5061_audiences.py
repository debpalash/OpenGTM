"""Add persistent workspace-scoped audiences.

Revision ID: 1c2d3e4f5061
Revises: 0b1c2d3e4f50
"""

import os
import re
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa

revision: str = "1c2d3e4f5061"
down_revision: Union[str, Sequence[str], None] = "0b1c2d3e4f50"
branch_labels = None
depends_on = None
APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")


def upgrade() -> None:
    op.create_table(
        "audiences",
        sa.Column("id", sa.String(), nullable=False),
        sa.Column("workspace_id", sa.String(), nullable=False),
        sa.Column("name", sa.String(length=200), nullable=False),
        sa.Column("description", sa.Text(), nullable=False, server_default=""),
        sa.Column("filters", sa.JSON(), nullable=False),
        sa.Column("member_count", sa.Integer(), nullable=False, server_default="0"),
        sa.Column("created_at", sa.DateTime(), nullable=False, server_default=sa.func.now()),
        sa.Column("updated_at", sa.DateTime(), nullable=False, server_default=sa.func.now()),
        sa.PrimaryKeyConstraint("id"),
        sa.UniqueConstraint("workspace_id", "name", name="uq_audiences_workspace_name"),
    )
    op.create_index("ix_audiences_workspace_id", "audiences", ["workspace_id"])
    op.create_index("ix_audiences_workspace_updated", "audiences", ["workspace_id", "updated_at"])

    if op.get_bind().dialect.name == "postgresql":
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", APP_DB_ROLE):
            raise RuntimeError("YUPCHA_APP_DB_ROLE is not a safe SQL identifier")
        op.execute(sa.text(f'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE "audiences" TO "{APP_DB_ROLE}"'))
        op.execute(sa.text('ALTER TABLE "audiences" ENABLE ROW LEVEL SECURITY'))
        op.execute(sa.text('ALTER TABLE "audiences" FORCE ROW LEVEL SECURITY'))
        op.execute(sa.text(
            'CREATE POLICY workspace_isolation ON "audiences" '
            "USING (workspace_id = current_setting('app.workspace_id', true)) "
            "WITH CHECK (workspace_id = current_setting('app.workspace_id', true))"
        ))


def downgrade() -> None:
    op.drop_index("ix_audiences_workspace_updated", table_name="audiences")
    op.drop_index("ix_audiences_workspace_id", table_name="audiences")
    op.drop_table("audiences")
