"""Add stable governance audit traversal index.

Revision ID: c35e9a7d42b8
Revises: b24d8f6c31a7
"""

from alembic import op

revision = "c35e9a7d42b8"
down_revision = "b24d8f6c31a7"
branch_labels = None
depends_on = None


def upgrade():
    op.create_index(
        "ix_governance_audit_ws_cursor", "governance_audit_events",
        ["workspace_id", "created_at", "id"],
    )


def downgrade():
    op.drop_index("ix_governance_audit_ws_cursor", table_name="governance_audit_events")
