"""Add stable connector-run traversal index and timestamp invariant.

Revision ID: e5701c9f64da
Revises: d46f0b8e53c9
"""

import sqlalchemy as sa
from alembic import op

revision = "e5701c9f64da"
down_revision = "d46f0b8e53c9"
branch_labels = None
depends_on = None


def upgrade():
    op.execute(sa.text(
        "UPDATE connector_runs SET created_at = CURRENT_TIMESTAMP WHERE created_at IS NULL",
    ))
    with op.batch_alter_table("connector_runs") as batch_op:
        batch_op.alter_column(
            "created_at",
            existing_type=sa.DateTime(timezone=True),
            existing_server_default=sa.func.now(),
            nullable=False,
        )
    op.create_index(
        "ix_connector_runs_ws_workbook_cursor", "connector_runs",
        ["workspace_id", "workbook_id", "created_at", "id"],
    )


def downgrade():
    op.drop_index("ix_connector_runs_ws_workbook_cursor", table_name="connector_runs")
    with op.batch_alter_table("connector_runs") as batch_op:
        batch_op.alter_column(
            "created_at",
            existing_type=sa.DateTime(timezone=True),
            existing_server_default=sa.func.now(),
            nullable=True,
        )
