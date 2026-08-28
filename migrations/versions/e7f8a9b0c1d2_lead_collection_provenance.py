"""Separate collection ownership from source provenance.

Revision ID: e7f8a9b0c1d2
Revises: d6e7f8a9b0c1
"""

from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


revision: str = "e7f8a9b0c1d2"
down_revision: Union[str, Sequence[str], None] = "d6e7f8a9b0c1"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    op.add_column("leads", sa.Column("source_url", sa.Text(), nullable=True))
    op.add_column(
        "leads",
        sa.Column("collection_job_id", sa.String(), nullable=True),
    )
    op.create_index(
        "ix_leads_ws_collection_job",
        "leads",
        ["workspace_id", "collection_job_id"],
        unique=False,
    )


def downgrade() -> None:
    op.drop_index("ix_leads_ws_collection_job", table_name="leads")
    op.drop_column("leads", "collection_job_id")
    op.drop_column("leads", "source_url")
