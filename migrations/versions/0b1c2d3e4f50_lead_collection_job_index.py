"""Reconcile the lead collection lookup index with ORM metadata.

Revision ID: 0b1c2d3e4f50
Revises: 0a1b2c3d4e5f
"""

from typing import Sequence, Union

from alembic import op


revision: str = "0b1c2d3e4f50"
down_revision: Union[str, Sequence[str], None] = "0a1b2c3d4e5f"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    op.create_index(
        "ix_leads_collection_job_id",
        "leads",
        ["collection_job_id"],
    )


def downgrade() -> None:
    op.drop_index("ix_leads_collection_job_id", table_name="leads")
