"""job_change watch kind: watch_subscriptions.config JSON column

Revision ID: e1f2a3b4c5d6
Revises: d0e1f2a3b4c5
Create Date: 2026-07-10 09:00:00.000000

Adds a nullable kind-specific ``config`` JSON column to ``watch_subscriptions``
(job_change: tracked-contact roster + per-poll cap). Per-contact detection
STATE stays in the existing ``cursor`` JSON — no new table, no RLS changes
(the column inherits the table's existing fail-closed workspace-isolation
policy from c7d8e9f0a1b2).
"""
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


# revision identifiers, used by Alembic.
revision: str = "e1f2a3b4c5d6"
down_revision: Union[str, Sequence[str], None] = "f8a9b0c1d2e3"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    op.add_column("watch_subscriptions", sa.Column("config", sa.JSON(), nullable=True))


def downgrade() -> None:
    op.drop_column("watch_subscriptions", "config")
