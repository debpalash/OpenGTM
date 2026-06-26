"""source reliability stats (sourcing P2)

Creates the global, non-tenant ``source_stats`` ledger of per-(source, region)
lead-source reliability. Mirrors ``provider_stats`` (enrichment-provider ledger):
no ``workspace_id``, so it sits outside the RLS tenancy model — it holds only
aggregate source health, no tenant PII.

Revision ID: f6a7b8c9d0e1
Revises: e5f6a7b8c9d0
Create Date: 2026-06-26

"""
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


# revision identifiers, used by Alembic.
revision: str = 'f6a7b8c9d0e1'
down_revision: Union[str, Sequence[str], None] = 'a7c1d2e3f4b5'
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    """Upgrade schema."""
    op.create_table(
        'source_stats',
        sa.Column('id', sa.Integer(), autoincrement=True, nullable=False),
        sa.Column('source', sa.String(length=100), nullable=False),
        sa.Column('region', sa.String(length=50), nullable=False),
        sa.Column('runs', sa.Integer(), nullable=True),
        sa.Column('runs_with_output', sa.Integer(), nullable=True),
        sa.Column('emitted', sa.Integer(), nullable=True),
        sa.Column('survived_dedup', sa.Integer(), nullable=True),
        sa.Column('validated', sa.Integer(), nullable=True),
        sa.Column('reliability', sa.Float(), nullable=True),
        sa.Column('updated_at', sa.DateTime(), server_default=sa.text('(CURRENT_TIMESTAMP)'), nullable=True),
        sa.PrimaryKeyConstraint('id'),
        sa.UniqueConstraint('source', 'region', name='uq_source_region'),
    )
    with op.batch_alter_table('source_stats', schema=None) as batch_op:
        batch_op.create_index(batch_op.f('ix_source_stats_source'), ['source'], unique=False)
        batch_op.create_index(batch_op.f('ix_source_stats_region'), ['region'], unique=False)


def downgrade() -> None:
    """Downgrade schema."""
    with op.batch_alter_table('source_stats', schema=None) as batch_op:
        batch_op.drop_index(batch_op.f('ix_source_stats_region'))
        batch_op.drop_index(batch_op.f('ix_source_stats_source'))

    op.drop_table('source_stats')
