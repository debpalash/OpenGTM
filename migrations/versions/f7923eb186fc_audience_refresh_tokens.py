"""Add bounded audience refresh correlation tokens.

Revision ID: f7923eb186fc
Revises: f6812da075eb
"""

import sqlalchemy as sa
from alembic import op

revision = "f7923eb186fc"
down_revision = "f6812da075eb"
branch_labels = None
depends_on = None


def upgrade():
    op.add_column("audience_members", sa.Column("refresh_token", sa.String(36)))
    op.add_column("audience_membership_events", sa.Column("refresh_id", sa.String(36)))
    op.create_index(
        "ix_audience_members_refresh_cursor", "audience_members",
        ["workspace_id", "audience_id", "refresh_token", "id"],
    )
    op.create_index(
        "ix_audience_events_refresh_cursor", "audience_membership_events",
        ["workspace_id", "audience_id", "refresh_id", "id"],
    )


def downgrade():
    op.drop_index("ix_audience_events_refresh_cursor", table_name="audience_membership_events")
    op.drop_index("ix_audience_members_refresh_cursor", table_name="audience_members")
    op.drop_column("audience_membership_events", "refresh_id")
    op.drop_column("audience_members", "refresh_token")
