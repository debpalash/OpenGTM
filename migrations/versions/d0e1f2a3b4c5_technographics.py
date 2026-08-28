"""add technographics to leads (structured website tech detection)

One new nullable JSON-text column on ``leads`` holding the structured website
technographics list ``[{name, category, source, confidence}]`` produced by the
tech_stack provider's homepage detection (docs/specs/research-wappalyzer-
technographics-spec.md). The legacy flat ``technologies`` comma-string column is
kept untouched for workbook/back-compat; this column adds category/source/
confidence for scoring + signal diffing.

Additive and backward-compatible:

  * NULL on existing rows — there is NO retroactive backfill (website tech is
    only detected on enrichment runs going forward, and only when
    TECH_STACK_WEBSITE_FETCH_ENABLED).
  * The column ships even with the flag OFF (cheap, inert) so enabling the flag
    later needs no schema redeploy.
  * NO new RLS DDL: ``leads`` already carries the workspace-scoped RLS policy
    (c42d0273d9bd); a new column on an existing RLS table inherits it, so this
    adds no cross-tenant surface.
  * SQLite-safe via batch_alter_table (mirrors c9d0e1f2a3b4 field_provenance).

Revision ID: d0e1f2a3b4c5
Revises: c9d0e1f2a3b4
Create Date: 2026-06-26
"""
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


# revision identifiers, used by Alembic.
revision: str = "d0e1f2a3b4c5"
down_revision: Union[str, Sequence[str], None] = "c9d0e1f2a3b4"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    """Upgrade schema."""
    with op.batch_alter_table("leads", schema=None) as batch_op:
        batch_op.add_column(
            sa.Column("technographics", sa.Text(), nullable=True)
        )


def downgrade() -> None:
    """Downgrade schema."""
    with op.batch_alter_table("leads", schema=None) as batch_op:
        batch_op.drop_column("technographics")
