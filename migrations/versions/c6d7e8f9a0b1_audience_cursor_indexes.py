"""Index stable audience member and event cursor traversal.

Revision ID: c6d7e8f9a0b1
Revises: b5c6d7e8f9a0
"""

from alembic import op

revision = "c6d7e8f9a0b1"
down_revision = "b5c6d7e8f9a0"
branch_labels = None
depends_on = None


def upgrade():
    op.create_index(
        "ix_audience_members_workspace_audience_cursor",
        "audience_members", ["workspace_id", "audience_id", "id"],
    )
    op.create_index(
        "ix_audience_events_workspace_audience_cursor",
        "audience_membership_events", ["workspace_id", "audience_id", "id"],
    )


def downgrade():
    op.drop_index("ix_audience_events_workspace_audience_cursor", table_name="audience_membership_events")
    op.drop_index("ix_audience_members_workspace_audience_cursor", table_name="audience_members")
