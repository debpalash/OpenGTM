"""Add workspace-scoped workbook action idempotency.

Revision ID: f9a0b1c2d3e4
Revises: e7f8a9b0c1d2
"""

from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


revision: str = "f9a0b1c2d3e4"
down_revision: Union[str, Sequence[str], None] = "e7f8a9b0c1d2"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    with op.batch_alter_table("workbooks") as batch:
        batch.add_column(
            sa.Column("action_idempotency_key", sa.String(length=255), nullable=True),
        )
        batch.create_unique_constraint(
            "uq_workbooks_workspace_action_key",
            ["workspace_id", "action_idempotency_key"],
        )


def downgrade() -> None:
    with op.batch_alter_table("workbooks") as batch:
        batch.drop_constraint(
            "uq_workbooks_workspace_action_key",
            type_="unique",
        )
        batch.drop_column("action_idempotency_key")
