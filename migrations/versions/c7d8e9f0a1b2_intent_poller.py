"""intent-poller: watch_subscriptions + poll_budget_ledger (RLS) + watch_schedules
mirror + jobs.fire_key single-flight index

Revision ID: c7d8e9f0a1b2
Revises: b2c3d4e5f6a7
Create Date: 2026-06-26 09:00:00.000000

Creates the Scheduled Intent-Signal Poller tables (spec §3):
  * watch_subscriptions  — RLS tenant table (per-company/feed watch + cursor)
  * poll_budget_ledger   — RLS tenant table (atomic per-(ws, UTC-day) budget)
  * watch_schedules      — NON-RLS ticker mirror (modelled on outreach_schedules)

Plus the jobs single-flight hardening (spec §3.4):
  * jobs.fire_key nullable column
  * uq_jobs_fire_key_active — PARTIAL unique index on active jobs
    (status IN ('pending','processing')) on Postgres; a plain index on SQLite.

Mirrors the b2c3d4e5f6a7 RLS recipe byte-for-byte: dialect-guarded GRANTs +
ENABLE/FORCE ROW LEVEL SECURITY + a fail-closed workspace-isolation policy on
app.workspace_id. SQLite/self-host gets the tables + a plain index only. The app
role is NOT (re)created — it already exists from c42d0273d9bd.
"""
import os
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa
from sqlalchemy import text


# revision identifiers, used by Alembic.
revision: str = "c7d8e9f0a1b2"
down_revision: Union[str, Sequence[str], None] = "b2c3d4e5f6a7"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None

APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")

# Tenant tables that get RLS (watch_schedules is intentionally NOT RLS).
_RLS_TABLES = ("watch_subscriptions", "poll_budget_ledger")
_ALL_TABLES = _RLS_TABLES + ("watch_schedules",)


def upgrade() -> None:
    is_pg = op.get_bind().dialect.name == "postgresql"

    op.create_table(
        "watch_subscriptions",
        sa.Column("id", sa.String(length=36), nullable=False),
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("kind", sa.String(length=20), nullable=False),
        sa.Column("target", sa.String(), nullable=False),
        sa.Column("resolved_cik", sa.String(length=10), nullable=True),
        sa.Column("lead_id", sa.Integer(), nullable=True),
        sa.Column("signal_types", sa.JSON(), nullable=True),
        sa.Column("interval", sa.String(length=10), nullable=True, server_default="daily"),
        sa.Column("schedule_anchor", sa.DateTime(timezone=True), nullable=True),
        sa.Column("enabled", sa.Boolean(), nullable=False, server_default=sa.true()),
        sa.Column("next_poll_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("cursor", sa.JSON(), nullable=True),
        sa.Column("last_polled_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("last_error", sa.String(), nullable=True),
        sa.Column("consecutive_failures", sa.Integer(), nullable=True, server_default="0"),
        sa.Column("created_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.Column("updated_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.PrimaryKeyConstraint("id"),
        sa.UniqueConstraint("workspace_id", "kind", "target", name="uq_watch_ws_kind_target"),
    )
    op.create_index("ix_watch_ws_enabled", "watch_subscriptions", ["workspace_id", "enabled"], unique=False)
    op.create_index("ix_watch_ws_kind", "watch_subscriptions", ["workspace_id", "kind"], unique=False)

    op.create_table(
        "poll_budget_ledger",
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("day", sa.Date(), nullable=False),
        sa.Column("n", sa.Integer(), nullable=False, server_default="0"),
        sa.Column("n_poll_now", sa.Integer(), nullable=False, server_default="0"),
        sa.PrimaryKeyConstraint("workspace_id", "day"),
    )

    # NON-RLS mirror (models outreach_schedules / scheduled_triggers).
    op.create_table(
        "watch_schedules",
        sa.Column("watch_id", sa.String(length=36), nullable=False),
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("next_poll_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("enabled", sa.Boolean(), nullable=False, server_default=sa.false()),
        sa.Column("updated_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.PrimaryKeyConstraint("watch_id"),
    )
    op.create_index("ix_watch_sched_due", "watch_schedules", ["enabled", "next_poll_at"], unique=False)

    # jobs single-flight hardening (spec §3.4).
    op.add_column("jobs", sa.Column("fire_key", sa.String(), nullable=True))
    if is_pg:
        # PARTIAL unique index — only active rows; NULL fire_keys are ignored.
        op.create_index(
            "uq_jobs_fire_key_active", "jobs", ["fire_key"], unique=True,
            postgresql_where=text("status IN ('pending','processing')"),
        )
    else:
        # SQLite: plain (non-unique) index — best-effort read-then-insert guard.
        op.create_index("ix_jobs_fire_key", "jobs", ["fire_key"], unique=False)

    if is_pg:
        _pg_upgrade()


def downgrade() -> None:
    is_pg = op.get_bind().dialect.name == "postgresql"
    if is_pg:
        _pg_downgrade()

    if is_pg:
        op.drop_index("uq_jobs_fire_key_active", table_name="jobs")
    else:
        op.drop_index("ix_jobs_fire_key", table_name="jobs")
    op.drop_column("jobs", "fire_key")

    op.drop_index("ix_watch_sched_due", table_name="watch_schedules")
    op.drop_table("watch_schedules")
    op.drop_table("poll_budget_ledger")
    op.drop_index("ix_watch_ws_kind", table_name="watch_subscriptions")
    op.drop_index("ix_watch_ws_enabled", table_name="watch_subscriptions")
    op.drop_table("watch_subscriptions")


def _pg_upgrade() -> None:
    """Postgres-only: least-privilege grants + RLS enable/force/policies.

    The app role already exists (created by c42d0273d9bd); here we only grant it
    DML on the new tables + sequence usage, then enable fail-closed RLS on the
    two tenant tables. watch_schedules gets DML grants but is intentionally NOT
    RLS-enabled (bootstrap reads it without a workspace GUC)."""
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
