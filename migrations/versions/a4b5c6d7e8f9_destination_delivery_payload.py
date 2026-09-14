"""Persist privacy-safe destination delivery payloads for reconciliation.

Revision ID: a4b5c6d7e8f9
Revises: f3a4b5c6d7e8
"""

from alembic import op
import sqlalchemy as sa


revision = "a4b5c6d7e8f9"
down_revision = "f3a4b5c6d7e8"
branch_labels = None
depends_on = None


def upgrade() -> None:
    op.add_column("destination_deliveries", sa.Column("payload", sa.JSON(), nullable=False, server_default="{}"))


def downgrade() -> None:
    op.drop_column("destination_deliveries", "payload")
