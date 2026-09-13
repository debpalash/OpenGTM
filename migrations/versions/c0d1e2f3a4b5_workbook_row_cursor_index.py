"""Add the stable workbook row cursor index.

Revision ID: c0d1e2f3a4b5
Revises: b6c7d8e9f0a1
"""

from typing import Sequence, Union

from alembic import op


revision: str = "c0d1e2f3a4b5"
down_revision: Union[str, Sequence[str], None] = "b6c7d8e9f0a1"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None

INDEX = "ix_workbook_rows_workbook_position_id"


def upgrade() -> None:
    op.create_index(INDEX, "workbook_rows", ["workbook_id", "position", "id"], unique=False)


def downgrade() -> None:
    op.drop_index(INDEX, table_name="workbook_rows")
