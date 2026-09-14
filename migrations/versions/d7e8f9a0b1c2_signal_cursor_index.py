"""Add compound signal traversal index.

Revision ID: d7e8f9a0b1c2
Revises: c6d7e8f9a0b1
"""

from alembic import op

revision = "d7e8f9a0b1c2"
down_revision = "c6d7e8f9a0b1"
branch_labels = None
depends_on = None


def upgrade():
    op.create_index(
        "ix_signals_ws_created_id",
        "signals",
        ["workspace_id", "created_at", "id"],
        unique=False,
    )


def downgrade():
    op.drop_index("ix_signals_ws_created_id", table_name="signals")
