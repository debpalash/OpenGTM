"""Repair server defaults on tables created early from ORM metadata.

Revision ID: d6e7f8a9b0c1
Revises: d5e6f7a8b9c0
Create Date: 2026-08-24

Some installations created this table through SQLAlchemy metadata before the
inbound migration ran.  Python-side defaults do not become database defaults,
so raw SQL inserts could violate the NOT NULL columns.  Make the schema
    contract explicit and idempotently repair those installations.  The same
    early-create path affected the two boolean defaults on automation triggers.
"""

from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


revision: str = "d6e7f8a9b0c1"
down_revision: Union[str, Sequence[str], None] = "d5e6f7a8b9c0"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    with op.batch_alter_table("outreach_inbound_schedules") as batch:
        batch.alter_column(
            "enabled",
            existing_type=sa.Boolean(),
            existing_nullable=False,
            server_default=sa.false(),
        )
        batch.alter_column(
            "consecutive_failures",
            existing_type=sa.Integer(),
            existing_nullable=False,
            server_default=sa.text("0"),
        )
    with op.batch_alter_table("triggers") as batch:
        batch.alter_column(
            "enabled",
            existing_type=sa.Boolean(),
            existing_nullable=False,
            server_default=sa.true(),
        )
        batch.alter_column(
            "stop_on_error",
            existing_type=sa.Boolean(),
            existing_nullable=False,
            server_default=sa.false(),
        )


def downgrade() -> None:
    with op.batch_alter_table("triggers") as batch:
        batch.alter_column(
            "stop_on_error",
            existing_type=sa.Boolean(),
            existing_nullable=False,
            server_default=None,
        )
        batch.alter_column(
            "enabled",
            existing_type=sa.Boolean(),
            existing_nullable=False,
            server_default=None,
        )
    with op.batch_alter_table("outreach_inbound_schedules") as batch:
        batch.alter_column(
            "consecutive_failures",
            existing_type=sa.Integer(),
            existing_nullable=False,
            server_default=None,
        )
        batch.alter_column(
            "enabled",
            existing_type=sa.Boolean(),
            existing_nullable=False,
            server_default=None,
        )
