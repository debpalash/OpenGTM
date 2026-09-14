"""Add compound watch traversal index.

Revision ID: e8f9a0b1c2d3
Revises: d7e8f9a0b1c2
"""

import sqlalchemy as sa
from alembic import op

revision = "e8f9a0b1c2d3"
down_revision = "d7e8f9a0b1c2"
branch_labels = None
depends_on = None


def upgrade():
    op.execute(sa.text(
        "UPDATE watch_subscriptions SET created_at = CURRENT_TIMESTAMP "
        "WHERE created_at IS NULL",
    ))
    with op.batch_alter_table("watch_subscriptions") as batch_op:
        batch_op.alter_column(
            "created_at",
            existing_type=sa.DateTime(timezone=True),
            existing_server_default=sa.func.now(),
            nullable=False,
        )
    op.create_index(
        "ix_watch_ws_created_id",
        "watch_subscriptions",
        ["workspace_id", "created_at", "id"],
        unique=False,
    )


def downgrade():
    op.drop_index("ix_watch_ws_created_id", table_name="watch_subscriptions")
    with op.batch_alter_table("watch_subscriptions") as batch_op:
        batch_op.alter_column(
            "created_at",
            existing_type=sa.DateTime(timezone=True),
            existing_server_default=sa.func.now(),
            nullable=True,
        )
