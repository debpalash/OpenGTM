"""Durable connector runs and workbook-row source identity.

Revision ID: c4d5e6f7a8b9
Revises: b3c4d5e6f7a8
Create Date: 2026-08-24
"""

import os
import re
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


revision: str = "c4d5e6f7a8b9"
down_revision: Union[str, Sequence[str], None] = "b3c4d5e6f7a8"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None

APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")


def upgrade() -> None:
    bind = op.get_bind()
    is_sqlite = bind.dialect.name == "sqlite"

    with op.batch_alter_table("workbook_rows") as batch:
        batch.add_column(sa.Column("source_provider", sa.String(length=100), nullable=True))
        batch.add_column(sa.Column("source_record_id", sa.String(length=255), nullable=True))
        batch.add_column(sa.Column("source_rank", sa.Integer(), nullable=True))
        batch.add_column(sa.Column("source_fetched_at", sa.DateTime(timezone=True), nullable=True))
        batch.create_index("ix_workbook_rows_source_provider", ["source_provider"])

    if bind.dialect.name == "postgresql":
        # Adopt source identities from legacy JSON snapshots. When historical
        # duplicates exist, only the first gets an identity so the new unique
        # constraint can be installed without deleting user data.
        op.execute(sa.text(
            """
            WITH candidates AS (
                SELECT id,
                       CASE
                         WHEN data->>'ambitionbox_company_id' IS NOT NULL THEN 'ambitionbox'
                         WHEN data->>'crm_external_id' IS NOT NULL THEN lower(coalesce(data->>'crm_type','crm'))
                         ELSE NULL
                       END AS provider,
                       coalesce(data->>'ambitionbox_company_id', data->>'crm_external_id') AS record_id
                FROM workbook_rows
            ), ranked AS (
                SELECT id, provider, record_id,
                       row_number() OVER (
                         PARTITION BY workbook_id, provider, record_id ORDER BY id
                       ) AS rn
                FROM workbook_rows JOIN candidates USING (id)
                WHERE provider IS NOT NULL AND record_id IS NOT NULL
            )
            UPDATE workbook_rows AS rows
            SET source_provider = ranked.provider,
                source_record_id = ranked.record_id
            FROM ranked
            WHERE rows.id = ranked.id AND ranked.rn = 1
            """
        ))

    with op.batch_alter_table("workbook_rows") as batch:
        batch.create_unique_constraint(
            "uq_workbook_row_source_identity",
            ["workbook_id", "source_provider", "source_record_id"],
        )

    op.create_table(
        "connector_runs",
        sa.Column("id", sa.String(), nullable=False),
        sa.Column("workspace_id", sa.String(), nullable=False),
        sa.Column("workbook_id", sa.String(), nullable=False),
        sa.Column("connector", sa.String(length=100), nullable=False),
        sa.Column("status", sa.String(length=32), nullable=False),
        sa.Column("query", sa.JSON(), nullable=True),
        sa.Column("cursor", sa.JSON(), nullable=True),
        sa.Column("requested_count", sa.Integer(), nullable=False),
        sa.Column("fetched_count", sa.Integer(), nullable=False),
        sa.Column("added_count", sa.Integer(), nullable=False),
        sa.Column("updated_count", sa.Integer(), nullable=False),
        sa.Column("skipped_count", sa.Integer(), nullable=False),
        sa.Column("pages_fetched", sa.Integer(), nullable=False),
        sa.Column("source_total", sa.Integer(), nullable=True),
        sa.Column("target_met", sa.Boolean(), nullable=False),
        sa.Column("exhausted", sa.Boolean(), nullable=False),
        sa.Column("error", sa.Text(), nullable=True),
        sa.Column("started_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("completed_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("created_at", sa.DateTime(timezone=True), server_default=sa.func.now()),
        sa.Column("updated_at", sa.DateTime(timezone=True), server_default=sa.func.now()),
        sa.ForeignKeyConstraint(["workbook_id"], ["workbooks.id"], ondelete="CASCADE"),
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_index("ix_connector_runs_workspace_id", "connector_runs", ["workspace_id"])
    op.create_index("ix_connector_runs_workbook_id", "connector_runs", ["workbook_id"])
    op.create_index("ix_connector_runs_connector", "connector_runs", ["connector"])
    op.create_index("ix_connector_runs_status", "connector_runs", ["status"])

    if bind.dialect.name == "postgresql":
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", APP_DB_ROLE):
            raise RuntimeError("YUPCHA_APP_DB_ROLE is not a safe SQL identifier")
        op.execute(sa.text(
            f'GRANT SELECT, INSERT, UPDATE, DELETE ON connector_runs TO "{APP_DB_ROLE}"'
        ))
        op.execute(sa.text("ALTER TABLE connector_runs ENABLE ROW LEVEL SECURITY"))
        op.execute(sa.text("ALTER TABLE connector_runs FORCE ROW LEVEL SECURITY"))
        op.execute(sa.text(
            "CREATE POLICY workspace_isolation ON connector_runs "
            "USING (workspace_id = current_setting('app.workspace_id', true)) "
            "WITH CHECK (workspace_id = current_setting('app.workspace_id', true))"
        ))


def downgrade() -> None:
    bind = op.get_bind()
    if bind.dialect.name == "postgresql":
        op.execute(sa.text("DROP POLICY IF EXISTS workspace_isolation ON connector_runs"))
        op.execute(sa.text("ALTER TABLE connector_runs NO FORCE ROW LEVEL SECURITY"))
        op.execute(sa.text("ALTER TABLE connector_runs DISABLE ROW LEVEL SECURITY"))
    op.drop_index("ix_connector_runs_status", table_name="connector_runs")
    op.drop_index("ix_connector_runs_connector", table_name="connector_runs")
    op.drop_index("ix_connector_runs_workbook_id", table_name="connector_runs")
    op.drop_index("ix_connector_runs_workspace_id", table_name="connector_runs")
    op.drop_table("connector_runs")
    with op.batch_alter_table("workbook_rows") as batch:
        batch.drop_constraint("uq_workbook_row_source_identity", type_="unique")
        batch.drop_index("ix_workbook_rows_source_provider")
        batch.drop_column("source_fetched_at")
        batch.drop_column("source_rank")
        batch.drop_column("source_record_id")
        batch.drop_column("source_provider")
