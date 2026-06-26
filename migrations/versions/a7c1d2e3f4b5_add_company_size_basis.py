"""add company_size_basis to leads

Provenance for the company-size heuristic signal (features/sourcing-company-size-
signals-spec.md). One new nullable scalar column on ``leads`` distinguishing a
known/exact size from a heuristic-estimated one ("exact" | "estimated:<signals>"
| ""). Additive and backward-compatible: existing rows default to "".

Revision ID: a7c1d2e3f4b5
Revises: e5f6a7b8c9d0
Create Date: 2026-06-26 13:00:00.000000

"""
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


# revision identifiers, used by Alembic.
revision: str = "a7c1d2e3f4b5"
down_revision: Union[str, Sequence[str], None] = "e5f6a7b8c9d0"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    """Upgrade schema."""
    with op.batch_alter_table("leads", schema=None) as batch_op:
        batch_op.add_column(
            sa.Column(
                "company_size_basis",
                sa.String(),
                nullable=True,
                server_default="",
            )
        )


def downgrade() -> None:
    """Downgrade schema."""
    with op.batch_alter_table("leads", schema=None) as batch_op:
        batch_op.drop_column("company_size_basis")
