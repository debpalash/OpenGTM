"""workbook_views: saved views (filters + sort + hidden columns) per workbook

Revision ID: e1f2a3b4c5d6
Revises: d0e1f2a3b4c5
Create Date: 2026-07-10

New RLS tenant table ``workbook_views`` — named presentation presets a user can
switch between in the workbook editor. Config JSON shape::

    {"filters": [{"column": "email", "op": "not_empty", "value": null}],
     "sort": [{"column": "score", "dir": "desc"}],
     "hidden_columns": ["notes"]}

Views are presentation-layer only (never mutate rows). The table follows the
workbooks RLS-hardening posture (e5f6a7b8c9d0) for child tables byte-for-byte:
denormalized NOT NULL ``workspace_id`` + indexes on both dialects, and on
Postgres least-privilege GRANTs + ENABLE/FORCE ROW LEVEL SECURITY + the
fail-closed workspace-isolation policy on ``app.workspace_id``. SQLite gets the
table + indexes only (RLS DDL is dialect-guarded). The app role is NOT
(re)created — it already exists from c42d0273d9bd.
"""
import os
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


# revision identifiers, used by Alembic.
revision: str = "f2a3b4c5d6e7"
down_revision: Union[str, Sequence[str], None] = "e1f2a3b4c5d6"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None

# Must match apps.api.core.config.settings.APP_DB_ROLE (and the head migrations).
APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")

TABLE = "workbook_views"


def upgrade() -> None:
    op.create_table(
        TABLE,
        sa.Column("id", sa.String(), nullable=False),
        sa.Column("workspace_id", sa.String(), nullable=False),
        sa.Column("workbook_id", sa.String(), nullable=False),
        sa.Column("name", sa.String(length=255), nullable=False),
        sa.Column("config", sa.JSON(), nullable=True),
        sa.Column("created_at", sa.DateTime(), server_default=sa.func.now(), nullable=True),
        sa.Column("updated_at", sa.DateTime(), server_default=sa.func.now(), nullable=True),
        sa.PrimaryKeyConstraint("id"),
        sa.ForeignKeyConstraint(["workbook_id"], ["workbooks.id"], ondelete="CASCADE"),
    )
    op.create_index(f"ix_{TABLE}_workspace_id", TABLE, ["workspace_id"], unique=False)
    op.create_index(f"ix_{TABLE}_workbook_id", TABLE, ["workbook_id"], unique=False)

    if op.get_bind().dialect.name == "postgresql":
        _pg_upgrade()


def _pg_upgrade() -> None:
    """Postgres-only: least-privilege grant + fail-closed RLS (e5f6a7b8c9d0 recipe)."""
    op.execute(f"GRANT SELECT, INSERT, UPDATE, DELETE ON {TABLE} TO {APP_DB_ROLE}")
    op.execute(f"ALTER TABLE {TABLE} ENABLE ROW LEVEL SECURITY")
    op.execute(f"ALTER TABLE {TABLE} FORCE ROW LEVEL SECURITY")
    # DROP-then-CREATE so a partially-applied / re-run state is safe (PG has no
    # CREATE POLICY IF NOT EXISTS).
    op.execute(f"DROP POLICY IF EXISTS {TABLE}_workspace_isolation ON {TABLE}")
    op.execute(
        f"""
        CREATE POLICY {TABLE}_workspace_isolation ON {TABLE}
        USING (workspace_id = current_setting('app.workspace_id', true))
        WITH CHECK (workspace_id = current_setting('app.workspace_id', true))
        """
    )


def downgrade() -> None:
    if op.get_bind().dialect.name == "postgresql":
        op.execute(f"DROP POLICY IF EXISTS {TABLE}_workspace_isolation ON {TABLE}")
        op.execute(f"ALTER TABLE {TABLE} NO FORCE ROW LEVEL SECURITY")
        op.execute(f"ALTER TABLE {TABLE} DISABLE ROW LEVEL SECURITY")
    op.drop_index(f"ix_{TABLE}_workbook_id", table_name=TABLE)
    op.drop_index(f"ix_{TABLE}_workspace_id", table_name=TABLE)
    op.drop_table(TABLE)
