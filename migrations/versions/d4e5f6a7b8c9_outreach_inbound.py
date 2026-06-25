"""outreach inbound: bounce/complaint ledger (RLS) + IMAP poll schedule mirror

Revision ID: d4e5f6a7b8c9
Revises: c7d8e9f0a1b2
Create Date: 2026-06-26 15:00:00.000000

Creates the two async-bounce/complaint-ingestion tables (bounce-ingestion spec):
  * outreach_inbound_messages — RLS tenant table (the dedup ledger / idempotency
    gate for apply_bounce; also the webhook idempotency gate).
  * outreach_inbound_schedules — NON-RLS per-workspace IMAP-poll mirror (modelled
    on outreach_schedules; read GUC-less at cold-start bootstrap).

Mirrors the b2c3d4e5f6a7 / c7d8e9f0a1b2 RLS recipe byte-for-byte: dialect-guarded
GRANTs + ENABLE/FORCE ROW LEVEL SECURITY + a fail-closed workspace-isolation
policy on app.workspace_id — but ONLY on outreach_inbound_messages. SQLite/self-
host gets the tables only. The app role is NOT (re)created (exists from
c42d0273d9bd).
"""
import os
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


# revision identifiers, used by Alembic.
revision: str = "d4e5f6a7b8c9"
down_revision: Union[str, Sequence[str], None] = "c7d8e9f0a1b2"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None

APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")

# RLS tenant table(s); the schedule mirror is intentionally NOT RLS.
_RLS_TABLES = ("outreach_inbound_messages",)
_ALL_TABLES = _RLS_TABLES + ("outreach_inbound_schedules",)


def upgrade() -> None:
    op.create_table(
        "outreach_inbound_messages",
        sa.Column("id", sa.Integer(), autoincrement=True, nullable=False),
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("imap_uid", sa.String(length=64), nullable=False, server_default=""),
        sa.Column("uidvalidity", sa.String(length=64), nullable=False, server_default=""),
        sa.Column("source_message_id", sa.String(), nullable=False, server_default=""),
        sa.Column("matched_send_id", sa.Integer(), nullable=True),
        sa.Column("kind", sa.String(length=20), nullable=True),
        sa.Column("recipient", sa.String(), nullable=True),
        sa.Column("diagnostic", sa.Text(), nullable=True),
        sa.Column("processed_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("created_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.PrimaryKeyConstraint("id"),
        sa.UniqueConstraint("workspace_id", "uidvalidity", "imap_uid", name="uq_inbound_ws_uid"),
        sa.UniqueConstraint("workspace_id", "source_message_id", name="uq_inbound_ws_srcmid"),
    )
    op.create_index("ix_inbound_ws_created", "outreach_inbound_messages", ["workspace_id", "created_at"], unique=False)

    # NON-RLS mirror (models outreach_schedules).
    op.create_table(
        "outreach_inbound_schedules",
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("next_poll_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("enabled", sa.Boolean(), nullable=False, server_default=sa.false()),
        sa.Column("consecutive_failures", sa.Integer(), nullable=False, server_default="0"),
        sa.Column("uidvalidity", sa.String(length=64), nullable=True),
        sa.Column("last_uid", sa.String(length=64), nullable=True),
        sa.Column("updated_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.PrimaryKeyConstraint("workspace_id"),
    )
    op.create_index("ix_inbound_sched_due", "outreach_inbound_schedules", ["enabled", "next_poll_at"], unique=False)

    if op.get_bind().dialect.name == "postgresql":
        _pg_upgrade()


def downgrade() -> None:
    if op.get_bind().dialect.name == "postgresql":
        _pg_downgrade()

    op.drop_index("ix_inbound_sched_due", table_name="outreach_inbound_schedules")
    op.drop_table("outreach_inbound_schedules")
    op.drop_index("ix_inbound_ws_created", table_name="outreach_inbound_messages")
    op.drop_table("outreach_inbound_messages")


def _pg_upgrade() -> None:
    """Postgres-only: least-privilege grants + RLS enable/force/policy.

    outreach_inbound_schedules gets DML grants but is intentionally NOT RLS-
    enabled (the cold-start bootstrap reads it without a workspace GUC)."""
    for tbl in _ALL_TABLES:
        op.execute(f"GRANT SELECT, INSERT, UPDATE, DELETE ON {tbl} TO {APP_DB_ROLE}")
    op.execute(f"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO {APP_DB_ROLE}")

    for tbl in _RLS_TABLES:
        op.execute(f"ALTER TABLE {tbl} ENABLE ROW LEVEL SECURITY")
        op.execute(f"ALTER TABLE {tbl} FORCE ROW LEVEL SECURITY")
        op.execute(
            f"""
            CREATE POLICY {tbl}_workspace_isolation ON {tbl}
            USING (workspace_id = current_setting('app.workspace_id', true))
            WITH CHECK (workspace_id = current_setting('app.workspace_id', true))
            """
        )


def _pg_downgrade() -> None:
    for tbl in _RLS_TABLES:
        op.execute(f"DROP POLICY IF EXISTS {tbl}_workspace_isolation ON {tbl}")
