"""Add stable destination history traversal indexes.

Revision ID: b24d8f6c31a7
Revises: a13c9e7b42d6
"""

from alembic import op

revision = "b24d8f6c31a7"
down_revision = "a13c9e7b42d6"
branch_labels = None
depends_on = None


def upgrade():
    op.create_index(
        "ix_destination_runs_ws_destination_cursor", "destination_runs",
        ["workspace_id", "destination_id", "created_at", "id"],
    )
    op.create_index(
        "ix_destination_inbound_ws_destination_cursor", "destination_inbound_receipts",
        ["workspace_id", "destination_id", "created_at", "id"],
    )


def downgrade():
    op.drop_index("ix_destination_inbound_ws_destination_cursor", table_name="destination_inbound_receipts")
    op.drop_index("ix_destination_runs_ws_destination_cursor", table_name="destination_runs")
