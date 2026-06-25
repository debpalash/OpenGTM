"""outreach: RLS-hardened sequences/enrollments/sends/suppressions + ticker mirror

Revision ID: b2c3d4e5f6a7
Revises: a1b2c3d4e5f6
Create Date: 2026-06-25 13:00:00.000000

Creates the five outreach tables (spec §3):
  outreach_sequences, outreach_enrollments, outreach_sends,
  outreach_suppressions (the four RLS tenant tables) and outreach_schedules
  (the non-RLS ticker mirror, modelled on scheduled_triggers).

Mirrors the head migrations (c42d0273d9bd / a1b2c3d4e5f6) RLS recipe byte-for-
byte: dialect-guarded GRANTs + ENABLE/FORCE ROW LEVEL SECURITY + a fail-closed
workspace-isolation policy on app.workspace_id. SQLite/self-host gets the tables
only. The app role is NOT (re)created — it already exists from c42d0273d9bd.
"""
import os
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


# revision identifiers, used by Alembic.
revision: str = "b2c3d4e5f6a7"
down_revision: Union[str, Sequence[str], None] = "a1b2c3d4e5f6"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None

APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")

# The four tenant tables that get RLS (outreach_schedules is intentionally NOT RLS).
_RLS_TABLES = (
    "outreach_sequences",
    "outreach_enrollments",
    "outreach_sends",
    "outreach_suppressions",
)
_ALL_TABLES = _RLS_TABLES + ("outreach_schedules",)


def upgrade() -> None:
    op.create_table(
        "outreach_sequences",
        sa.Column("id", sa.String(length=36), nullable=False),
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("name", sa.String(length=200), nullable=False),
        sa.Column("description", sa.Text(), nullable=True),
        sa.Column("steps", sa.JSON(), nullable=False),
        sa.Column("status", sa.String(length=20), nullable=True),
        sa.Column("daily_limit", sa.Integer(), nullable=True),
        sa.Column("send_window_start", sa.Integer(), nullable=True),
        sa.Column("send_window_end", sa.Integer(), nullable=True),
        sa.Column("send_window_tz", sa.String(length=40), nullable=True),
        sa.Column("consent_basis", sa.String(length=40), nullable=False, server_default=""),
        sa.Column("bounce_count", sa.Integer(), nullable=True, server_default="0"),
        sa.Column("complaint_count", sa.Integer(), nullable=True, server_default="0"),
        sa.Column("auto_paused", sa.Boolean(), nullable=True, server_default=sa.false()),
        sa.Column("created_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.Column("updated_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_index("ix_outreach_seq_ws", "outreach_sequences", ["workspace_id"], unique=False)
    op.create_index("ix_outreach_seq_ws_status", "outreach_sequences", ["workspace_id", "status"], unique=False)

    op.create_table(
        "outreach_enrollments",
        sa.Column("id", sa.Integer(), autoincrement=True, nullable=False),
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("sequence_id", sa.String(length=36), nullable=False),
        sa.Column("lead_id", sa.Integer(), nullable=False),
        sa.Column("to_email_snapshot", sa.String(), nullable=False),
        sa.Column("consent_source", sa.String(length=120), nullable=True),
        sa.Column("consent_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("current_step", sa.Integer(), nullable=True),
        sa.Column("status", sa.String(length=20), nullable=True),
        sa.Column("next_send_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("sent_count", sa.Integer(), nullable=True),
        sa.Column("soft_bounce_count", sa.Integer(), nullable=True),
        sa.Column("last_sent_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("error", sa.Text(), nullable=True),
        sa.PrimaryKeyConstraint("id"),
        sa.UniqueConstraint("workspace_id", "sequence_id", "lead_id", name="uq_enroll_ws_seq_lead"),
    )
    op.create_index("ix_enroll_ws_seq_status", "outreach_enrollments", ["workspace_id", "sequence_id", "status"], unique=False)
    op.create_index("ix_enroll_ws_due", "outreach_enrollments", ["workspace_id", "status", "next_send_at"], unique=False)

    op.create_table(
        "outreach_sends",
        sa.Column("id", sa.Integer(), autoincrement=True, nullable=False),
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("sequence_id", sa.String(length=36), nullable=True),
        sa.Column("enrollment_id", sa.Integer(), nullable=True),
        sa.Column("lead_id", sa.Integer(), nullable=True),
        sa.Column("step_number", sa.Integer(), nullable=True),
        sa.Column("to_email", sa.String(), nullable=False),
        sa.Column("subject", sa.String(), nullable=True),
        sa.Column("status", sa.String(length=20), nullable=True),
        sa.Column("skip_reason", sa.String(length=40), nullable=True),
        sa.Column("message_id", sa.String(), nullable=True),
        sa.Column("idempotency_key", sa.String(length=255), nullable=False),
        sa.Column("charged_usd", sa.Float(), nullable=True),
        sa.Column("migrated", sa.Boolean(), nullable=True, server_default=sa.false()),
        sa.Column("error", sa.Text(), nullable=True),
        sa.Column("sent_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("opened_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("replied_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("bounced_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("created_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.Column("updated_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.PrimaryKeyConstraint("id"),
        sa.UniqueConstraint("workspace_id", "idempotency_key", name="uq_outreach_send_idem"),
    )
    op.create_index("ix_send_ws_seq_created", "outreach_sends", ["workspace_id", "sequence_id", "created_at"], unique=False)
    op.create_index("ix_send_ws_status", "outreach_sends", ["workspace_id", "status"], unique=False)
    op.create_index("ix_send_ws_msgid", "outreach_sends", ["workspace_id", "message_id"], unique=False)
    op.create_index("ix_send_ws_sent_at", "outreach_sends", ["workspace_id", "sent_at"], unique=False)

    op.create_table(
        "outreach_suppressions",
        sa.Column("id", sa.Integer(), autoincrement=True, nullable=False),
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("email", sa.String(), nullable=False),
        sa.Column("reason", sa.String(length=20), nullable=True),
        sa.Column("source", sa.String(length=40), nullable=True),
        sa.Column("locked", sa.Boolean(), nullable=True, server_default=sa.false()),
        sa.Column("created_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.PrimaryKeyConstraint("id"),
        sa.UniqueConstraint("workspace_id", "email", name="uq_suppress_ws_email"),
    )
    op.create_index("ix_suppress_ws_email", "outreach_suppressions", ["workspace_id", "email"], unique=False)

    # NON-RLS mirror (models scheduled_triggers).
    op.create_table(
        "outreach_schedules",
        sa.Column("sequence_id", sa.String(length=36), nullable=False),
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("next_tick_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("enabled", sa.Boolean(), nullable=False, server_default=sa.false()),
        sa.Column("updated_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.PrimaryKeyConstraint("sequence_id"),
    )
    op.create_index("ix_outreach_sched_due", "outreach_schedules", ["enabled", "next_tick_at"], unique=False)

    if op.get_bind().dialect.name == "postgresql":
        _pg_upgrade()


def downgrade() -> None:
    if op.get_bind().dialect.name == "postgresql":
        _pg_downgrade()

    op.drop_index("ix_outreach_sched_due", table_name="outreach_schedules")
    op.drop_table("outreach_schedules")
    op.drop_index("ix_suppress_ws_email", table_name="outreach_suppressions")
    op.drop_table("outreach_suppressions")
    op.drop_index("ix_send_ws_sent_at", table_name="outreach_sends")
    op.drop_index("ix_send_ws_msgid", table_name="outreach_sends")
    op.drop_index("ix_send_ws_status", table_name="outreach_sends")
    op.drop_index("ix_send_ws_seq_created", table_name="outreach_sends")
    op.drop_table("outreach_sends")
    op.drop_index("ix_enroll_ws_due", table_name="outreach_enrollments")
    op.drop_index("ix_enroll_ws_seq_status", table_name="outreach_enrollments")
    op.drop_table("outreach_enrollments")
    op.drop_index("ix_outreach_seq_ws_status", table_name="outreach_sequences")
    op.drop_index("ix_outreach_seq_ws", table_name="outreach_sequences")
    op.drop_table("outreach_sequences")


def _pg_upgrade() -> None:
    """Postgres-only: least-privilege grants + RLS enable/force/policies.

    The app role already exists (created by c42d0273d9bd); here we only grant it
    DML on the new tables + sequence usage, then enable fail-closed RLS on the
    four tenant tables. outreach_schedules gets DML grants but is intentionally
    NOT RLS-enabled (the ticker bootstrap reads it without a workspace GUC).
    """
    for tbl in _ALL_TABLES:
        op.execute(f"GRANT SELECT, INSERT, UPDATE, DELETE ON {tbl} TO {APP_DB_ROLE}")
    # Serial PKs on enrollments / sends / suppressions need the sequences.
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
    """Drop the RLS policies before the tables disappear. App role is not dropped
    (matches the head migrations — it may be shared / granted to login roles)."""
    for tbl in _RLS_TABLES:
        op.execute(f"DROP POLICY IF EXISTS {tbl}_workspace_isolation ON {tbl}")
