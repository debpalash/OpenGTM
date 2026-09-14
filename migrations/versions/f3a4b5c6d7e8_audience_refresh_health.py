"""Persist audience refresh health and failure diagnostics.

Revision ID: f3a4b5c6d7e8
Revises: e2f3a4b5c6d7
"""

from alembic import op
import sqlalchemy as sa


revision = "f3a4b5c6d7e8"
down_revision = "e2f3a4b5c6d7"
branch_labels = None
depends_on = None


def upgrade() -> None:
    op.add_column("audiences", sa.Column("refresh_health", sa.String(20), nullable=False, server_default="unverified"))
    op.add_column("audiences", sa.Column("last_refresh_error", sa.Text(), nullable=True))
    op.add_column("audiences", sa.Column("consecutive_refresh_failures", sa.Integer(), nullable=False, server_default="0"))


def downgrade() -> None:
    op.drop_column("audiences", "consecutive_refresh_failures")
    op.drop_column("audiences", "last_refresh_error")
    op.drop_column("audiences", "refresh_health")
