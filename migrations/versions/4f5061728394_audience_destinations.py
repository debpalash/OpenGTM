"""Add durable audience destination sync infrastructure.

Revision ID: 4f5061728394
Revises: 3e4f50617283
"""

import os
import re
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa

revision: str = "4f5061728394"
down_revision: Union[str, Sequence[str], None] = "3e4f50617283"
branch_labels = None
depends_on = None
APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")
TABLES = ("audience_destinations", "destination_runs", "destination_deliveries")


def upgrade() -> None:
    op.create_table(
        "audience_destinations",
        sa.Column("id", sa.String(), nullable=False),
        sa.Column("workspace_id", sa.String(), nullable=False),
        sa.Column("audience_id", sa.String(), nullable=False),
        sa.Column("name", sa.String(length=200), nullable=False),
        sa.Column("destination_type", sa.String(length=32), nullable=False),
        sa.Column("enabled", sa.Boolean(), nullable=False, server_default=sa.true()),
        sa.Column("config", sa.JSON(), nullable=False),
        sa.Column("field_map", sa.JSON(), nullable=False),
        sa.Column("health_status", sa.String(length=20), nullable=False, server_default="unverified"),
        sa.Column("last_error", sa.Text(), nullable=True),
        sa.Column("last_success_at", sa.DateTime(), nullable=True),
        sa.Column("created_at", sa.DateTime(), nullable=False, server_default=sa.func.now()),
        sa.Column("updated_at", sa.DateTime(), nullable=False, server_default=sa.func.now()),
        sa.ForeignKeyConstraint(["audience_id"], ["audiences.id"], ondelete="CASCADE"),
        sa.PrimaryKeyConstraint("id"),
        sa.UniqueConstraint("workspace_id", "audience_id", "name", name="uq_audience_destinations_name"),
    )
    op.create_index("ix_audience_destinations_workspace_id", "audience_destinations", ["workspace_id"])
    op.create_index("ix_audience_destinations_audience_id", "audience_destinations", ["audience_id"])
    op.create_index("ix_audience_destinations_workspace_audience", "audience_destinations", ["workspace_id", "audience_id"])
    op.create_table(
        "destination_runs",
        sa.Column("id", sa.String(), nullable=False),
        sa.Column("workspace_id", sa.String(), nullable=False),
        sa.Column("destination_id", sa.String(), nullable=False),
        sa.Column("status", sa.String(length=20), nullable=False, server_default="pending"),
        sa.Column("requested_by", sa.String(), nullable=True),
        sa.Column("attempted", sa.Integer(), nullable=False, server_default="0"),
        sa.Column("succeeded", sa.Integer(), nullable=False, server_default="0"),
        sa.Column("failed", sa.Integer(), nullable=False, server_default="0"),
        sa.Column("skipped", sa.Integer(), nullable=False, server_default="0"),
        sa.Column("error", sa.Text(), nullable=True),
        sa.Column("started_at", sa.DateTime(), nullable=True),
        sa.Column("finished_at", sa.DateTime(), nullable=True),
        sa.Column("created_at", sa.DateTime(), nullable=False, server_default=sa.func.now()),
        sa.ForeignKeyConstraint(["destination_id"], ["audience_destinations.id"], ondelete="CASCADE"),
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_index("ix_destination_runs_workspace_id", "destination_runs", ["workspace_id"])
    op.create_index("ix_destination_runs_destination_id", "destination_runs", ["destination_id"])
    op.create_index("ix_destination_runs_workspace_destination_created", "destination_runs", ["workspace_id", "destination_id", "created_at"])
    op.create_table(
        "destination_deliveries",
        sa.Column("id", sa.Integer(), autoincrement=True, nullable=False),
        sa.Column("workspace_id", sa.String(), nullable=False),
        sa.Column("run_id", sa.String(), nullable=False),
        sa.Column("destination_id", sa.String(), nullable=False),
        sa.Column("lead_id", sa.Integer(), nullable=False),
        sa.Column("operation", sa.String(length=16), nullable=False),
        sa.Column("idempotency_key", sa.String(length=255), nullable=False),
        sa.Column("payload_fingerprint", sa.String(length=64), nullable=False),
        sa.Column("status", sa.String(length=20), nullable=False),
        sa.Column("attempts", sa.Integer(), nullable=False),
        sa.Column("external_id", sa.String(), nullable=True),
        sa.Column("summary", sa.Text(), nullable=True),
        sa.Column("error", sa.Text(), nullable=True),
        sa.Column("delivered_at", sa.DateTime(), nullable=True),
        sa.Column("created_at", sa.DateTime(), nullable=False, server_default=sa.func.now()),
        sa.Column("updated_at", sa.DateTime(), nullable=False, server_default=sa.func.now()),
        sa.ForeignKeyConstraint(["run_id"], ["destination_runs.id"], ondelete="CASCADE"),
        sa.ForeignKeyConstraint(["destination_id"], ["audience_destinations.id"], ondelete="CASCADE"),
        sa.PrimaryKeyConstraint("id"),
        sa.UniqueConstraint("workspace_id", "idempotency_key", name="uq_destination_delivery_idem"),
    )
    for name, columns in (
        ("ix_destination_deliveries_workspace_id", ["workspace_id"]),
        ("ix_destination_deliveries_run_id", ["run_id"]),
        ("ix_destination_deliveries_destination_id", ["destination_id"]),
        ("ix_destination_deliveries_lead_id", ["lead_id"]),
        ("ix_destination_deliveries_workspace_run", ["workspace_id", "run_id"]),
    ):
        op.create_index(name, "destination_deliveries", columns)

    if op.get_bind().dialect.name == "postgresql":
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", APP_DB_ROLE):
            raise RuntimeError("YUPCHA_APP_DB_ROLE is not a safe SQL identifier")
        for table in TABLES:
            op.execute(sa.text(f'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE "{table}" TO "{APP_DB_ROLE}"'))
            op.execute(sa.text(f'ALTER TABLE "{table}" ENABLE ROW LEVEL SECURITY'))
            op.execute(sa.text(f'ALTER TABLE "{table}" FORCE ROW LEVEL SECURITY'))
            op.execute(sa.text(
                f'CREATE POLICY workspace_isolation ON "{table}" '
                "USING (workspace_id = current_setting('app.workspace_id', true)) "
                "WITH CHECK (workspace_id = current_setting('app.workspace_id', true))"
            ))
        op.execute(sa.text(f'GRANT USAGE, SELECT ON SEQUENCE "destination_deliveries_id_seq" TO "{APP_DB_ROLE}"'))


def downgrade() -> None:
    op.drop_table("destination_deliveries")
    op.drop_table("destination_runs")
    op.drop_table("audience_destinations")
