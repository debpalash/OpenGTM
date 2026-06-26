"""source health (sourcing P2/P3 — active-probe rolling per-source health)

Creates the global, non-tenant ``source_health`` table: one row per registry
``site:`` source (PK = source ``name``), holding rolling probe yield + a
disable/re-enable state machine. Like ``source_stats`` / ``provider_stats`` it has
**no ``workspace_id`` and no RLS** — it is shared infra state (aggregate source
health, no tenant PII), and lives in the same non-RLS DB as the Job queue so it is
shared across worker replicas. Additive, backward-safe DDL.

Revision ID: a7b8c9d0e1f2
Revises: f6a7b8c9d0e1
Create Date: 2026-06-26

"""
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


# revision identifiers, used by Alembic.
revision: str = 'a7b8c9d0e1f2'
down_revision: Union[str, Sequence[str], None] = 'f6a7b8c9d0e1'
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    """Upgrade schema."""
    op.create_table(
        'source_health',
        sa.Column('name', sa.String(length=100), nullable=False),
        sa.Column('state', sa.String(length=20), nullable=False, server_default='healthy'),
        sa.Column('consecutive_zero', sa.Integer(), nullable=False, server_default='0'),
        sa.Column('consecutive_nonzero', sa.Integer(), nullable=False, server_default='0'),
        sa.Column('last_yield', sa.Integer(), nullable=True),
        sa.Column('ewma_yield', sa.Float(), nullable=True),
        sa.Column('probes', sa.Integer(), nullable=False, server_default='0'),
        sa.Column('last_probe_at', sa.DateTime(), nullable=True),
        sa.Column('last_ok_at', sa.DateTime(), nullable=True),
        sa.Column('last_outage_at', sa.DateTime(), nullable=True),
        sa.Column('disabled_at', sa.DateTime(), nullable=True),
        sa.Column('disabled_reason', sa.String(length=50), nullable=True),
        sa.Column('manual_override', sa.Boolean(), nullable=False, server_default=sa.false()),
        sa.Column('updated_at', sa.DateTime(), server_default=sa.text('(CURRENT_TIMESTAMP)'), nullable=True),
        sa.PrimaryKeyConstraint('name'),
    )


def downgrade() -> None:
    """Downgrade schema."""
    op.drop_table('source_health')
