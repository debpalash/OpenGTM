"""add field_provenance to leads (per-fact provenance)

One new nullable JSON-text column on ``leads`` holding per-fact provenance
``{field_name: {source, license, confidence, fetched_at}}`` (docs/specs/research-
per-fact-provenance-spec.md). Additive and backward-compatible:

  * NULL on existing rows — there is NO retroactive backfill (historical
    provenance is unknown; stamping now() would lie about freshness). New
    enrichment writes populate it going forward when PROVENANCE_TRACKING_ENABLED.
  * The column ships even with the flag OFF (cheap, inert) so enabling the flag
    later needs no schema redeploy.
  * NO new RLS DDL: ``leads`` already carries the workspace-scoped RLS policy
    (c42d0273d9bd); a new column on an existing RLS table inherits it, so this
    adds no cross-tenant surface.
  * SQLite-safe via batch_alter_table (mirrors the company_size_basis migration).

Revision ID: c9d0e1f2a3b4
Revises: b8c9d0e1f2a3
Create Date: 2026-06-26
"""
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


# revision identifiers, used by Alembic.
revision: str = "c9d0e1f2a3b4"
down_revision: Union[str, Sequence[str], None] = "b8c9d0e1f2a3"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    """Upgrade schema."""
    with op.batch_alter_table("leads", schema=None) as batch_op:
        batch_op.add_column(
            sa.Column("field_provenance", sa.Text(), nullable=True)
        )


def downgrade() -> None:
    """Downgrade schema."""
    with op.batch_alter_table("leads", schema=None) as batch_op:
        batch_op.drop_column("field_provenance")
