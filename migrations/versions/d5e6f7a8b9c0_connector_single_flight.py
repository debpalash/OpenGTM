"""Enforce one active connector run per workbook and connector.

Revision ID: d5e6f7a8b9c0
Revises: c4d5e6f7a8b9
Create Date: 2026-08-24
"""

from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


revision: str = "d5e6f7a8b9c0"
down_revision: Union[str, Sequence[str], None] = "c4d5e6f7a8b9"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None

_ACTIVE = "status IN ('pending', 'running', 'retrying')"


def upgrade() -> None:
    bind = op.get_bind()
    duplicates = bind.execute(sa.text(
        "SELECT workbook_id, connector, count(*) "
        "FROM connector_runs WHERE " + _ACTIVE + " "
        "GROUP BY workbook_id, connector HAVING count(*) > 1"
    )).fetchall()
    if duplicates:
        examples = ", ".join(
            f"{workbook_id}/{connector}={count}"
            for workbook_id, connector, count in duplicates[:5]
        )
        raise RuntimeError(
            "connector single-flight migration aborted; resolve duplicate "
            f"active runs first: {examples}"
        )

    op.create_index(
        "uq_connector_runs_active",
        "connector_runs",
        ["workbook_id", "connector"],
        unique=True,
        postgresql_where=sa.text(_ACTIVE),
        sqlite_where=sa.text(_ACTIVE),
    )


def downgrade() -> None:
    op.drop_index("uq_connector_runs_active", table_name="connector_runs")
