"""Add expiry and use telemetry to destination inbound credentials.

Revision ID: b5c6d7e8f9a0
Revises: a4b5c6d7e8f9
"""

from alembic import op
import sqlalchemy as sa

revision = "b5c6d7e8f9a0"
down_revision = "a4b5c6d7e8f9"
branch_labels = None
depends_on = None


def upgrade():
    with op.batch_alter_table("destination_inbound_tokens") as batch:
        batch.add_column(sa.Column("expires_at", sa.DateTime(timezone=True), nullable=True))
        batch.add_column(sa.Column("last_used_at", sa.DateTime(timezone=True), nullable=True))


def downgrade():
    with op.batch_alter_table("destination_inbound_tokens") as batch:
        batch.drop_column("last_used_at")
        batch.drop_column("expires_at")
