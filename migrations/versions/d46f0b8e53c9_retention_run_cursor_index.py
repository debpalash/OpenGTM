"""Add stable retention-run traversal index.

Revision ID: d46f0b8e53c9
Revises: c35e9a7d42b8
"""

from alembic import op

revision = "d46f0b8e53c9"
down_revision = "c35e9a7d42b8"
branch_labels = None
depends_on = None


def upgrade():
    op.create_index(
        "ix_retention_runs_ws_cursor", "retention_runs",
        ["workspace_id", "created_at", "id"],
    )


def downgrade():
    op.drop_index("ix_retention_runs_ws_cursor", table_name="retention_runs")
