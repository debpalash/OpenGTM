"""automations: trigger engine tables + RLS

Revision ID: a1b2c3d4e5f6
Revises: c42d0273d9bd
Create Date: 2026-06-25 12:00:00.000000

Creates the five automations tables (§3.11 of the trigger-engine spec):
  triggers, trigger_runs, trigger_action_results, trigger_cap_reservations
  (the four RLS tenant tables) and scheduled_triggers (the non-RLS mirror).

Mirrors the head migration (c42d0273d9bd) RLS pattern byte-for-byte: dialect-
guarded GRANTs + ENABLE/FORCE ROW LEVEL SECURITY + a fail-closed workspace
isolation policy on app.workspace_id. SQLite/self-host gets the tables only.
"""
import os
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


# revision identifiers, used by Alembic.
revision: str = 'a1b2c3d4e5f6'
down_revision: Union[str, Sequence[str], None] = 'c42d0273d9bd'
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


# Must match apps.api.core.config.settings.APP_DB_ROLE and the head migration.
APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")

# The four tenant tables that get RLS (scheduled_triggers is intentionally NOT RLS).
_RLS_TABLES = ("triggers", "trigger_runs", "trigger_action_results", "trigger_cap_reservations")
_ALL_TABLES = _RLS_TABLES + ("scheduled_triggers",)


def upgrade() -> None:
    """Upgrade schema."""
    op.create_table(
        "triggers",
        sa.Column("id", sa.String(length=36), nullable=False),
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("name", sa.String(length=200), nullable=False),
        sa.Column("enabled", sa.Boolean(), nullable=False, server_default=sa.true()),
        sa.Column("trigger_type", sa.String(length=32), nullable=False),
        sa.Column("trigger_config", sa.JSON(), nullable=False),
        sa.Column("condition", sa.Text(), nullable=True),
        sa.Column("actions", sa.JSON(), nullable=False),
        sa.Column("scope_workbook_ids", sa.JSON(), nullable=True),
        sa.Column("stop_on_error", sa.Boolean(), nullable=False, server_default=sa.false()),
        sa.Column("max_spend_usd_per_day", sa.Float(), nullable=True),
        sa.Column("max_actions_per_day", sa.Integer(), nullable=True),
        sa.Column("next_run_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("schedule_anchor", sa.DateTime(timezone=True), nullable=True),
        sa.Column("last_fired_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("created_by", sa.String(length=64), nullable=True),
        sa.Column("created_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.Column("updated_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_index("ix_triggers_workspace_id", "triggers", ["workspace_id"], unique=False)
    op.create_index("ix_triggers_ws_type", "triggers", ["workspace_id", "trigger_type"], unique=False)
    op.create_index("ix_triggers_ws_enabled", "triggers", ["workspace_id", "enabled"], unique=False)
    op.create_index("ix_triggers_ws_next_run", "triggers", ["workspace_id", "trigger_type", "next_run_at"], unique=False)

    op.create_table(
        "trigger_runs",
        sa.Column("id", sa.String(length=36), nullable=False),
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("trigger_id", sa.String(length=36), nullable=False),
        sa.Column("trigger_name_snapshot", sa.String(length=200), nullable=True),
        sa.Column("job_id", sa.Integer(), nullable=True),
        sa.Column("status", sa.String(length=20), nullable=True),
        sa.Column("fire_source", sa.String(length=32), nullable=True),
        sa.Column("fire_key", sa.String(length=200), nullable=True),
        sa.Column("sweep_cursor", sa.String(length=200), nullable=True),
        sa.Column("matched_rows", sa.Integer(), nullable=True),
        sa.Column("actions_attempted", sa.Integer(), nullable=True),
        sa.Column("actions_succeeded", sa.Integer(), nullable=True),
        sa.Column("actions_skipped", sa.Integer(), nullable=True),
        sa.Column("actions_failed", sa.Integer(), nullable=True),
        sa.Column("total_charged_usd", sa.Float(), nullable=True),
        sa.Column("error", sa.Text(), nullable=True),
        sa.Column("started_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.Column("finished_at", sa.DateTime(timezone=True), nullable=True),
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_index("ix_trigger_runs_ws", "trigger_runs", ["workspace_id"], unique=False)
    op.create_index(
        "ix_trigger_runs_ws_trigger_started", "trigger_runs",
        ["workspace_id", "trigger_id", "started_at"], unique=False,
    )

    op.create_table(
        "trigger_action_results",
        sa.Column("id", sa.Integer(), autoincrement=True, nullable=False),
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("run_id", sa.String(length=36), nullable=False),
        sa.Column("trigger_id", sa.String(length=36), nullable=False),
        sa.Column("workbook_id", sa.String(length=36), nullable=False),
        sa.Column("row_id", sa.String(length=36), nullable=False),
        sa.Column("lead_id", sa.Integer(), nullable=True),
        sa.Column("action_index", sa.Integer(), nullable=False),
        sa.Column("action_type", sa.String(length=32), nullable=False),
        sa.Column("idempotency_key", sa.String(length=255), nullable=False),
        sa.Column("status", sa.String(length=20), nullable=True),
        sa.Column("skip_reason", sa.String(length=40), nullable=True),
        sa.Column("charged_usd", sa.Float(), nullable=True),
        sa.Column("result_summary", sa.Text(), nullable=True),
        sa.Column("error", sa.Text(), nullable=True),
        sa.Column("created_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.Column("updated_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.PrimaryKeyConstraint("id"),
        sa.UniqueConstraint("workspace_id", "idempotency_key", name="uq_trigger_action_idem"),
    )
    op.create_index(
        "ix_tar_ws_trigger_created", "trigger_action_results",
        ["workspace_id", "trigger_id", "created_at"], unique=False,
    )
    op.create_index("ix_tar_ws_created", "trigger_action_results", ["workspace_id", "created_at"], unique=False)

    op.create_table(
        "trigger_cap_reservations",
        sa.Column("id", sa.Integer(), autoincrement=True, nullable=False),
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("trigger_id", sa.String(length=36), nullable=False),
        sa.Column("day_utc", sa.String(length=10), nullable=False),
        sa.Column("reserved_usd", sa.Float(), nullable=False),
        sa.Column("reserved_actions", sa.Integer(), nullable=False, server_default="1"),
        sa.Column("idempotency_key", sa.String(length=255), nullable=False),
        sa.Column("state", sa.String(length=12), nullable=False, server_default="held"),
        sa.Column("created_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.PrimaryKeyConstraint("id"),
        sa.UniqueConstraint("workspace_id", "idempotency_key", name="uq_cap_reservation_idem"),
    )
    op.create_index(
        "ix_capres_ws_trig_day", "trigger_cap_reservations",
        ["workspace_id", "trigger_id", "day_utc", "state"], unique=False,
    )
    op.create_index(
        "ix_capres_ws_day", "trigger_cap_reservations",
        ["workspace_id", "day_utc", "state"], unique=False,
    )

    # Non-RLS mirror for cold-start scheduler bootstrap.
    op.create_table(
        "scheduled_triggers",
        sa.Column("trigger_id", sa.String(length=36), nullable=False),
        sa.Column("workspace_id", sa.String(length=64), nullable=False),
        sa.Column("next_run_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("enabled", sa.Boolean(), nullable=False, server_default=sa.true()),
        sa.Column("updated_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=True),
        sa.PrimaryKeyConstraint("trigger_id"),
    )
    op.create_index("ix_scheduled_triggers_due", "scheduled_triggers", ["enabled", "next_run_at"], unique=False)

    if op.get_bind().dialect.name == "postgresql":
        _pg_upgrade()


def _pg_upgrade() -> None:
    """Postgres-only: least-privilege grants + RLS enable/force/policies.

    Mirrors c42d0273d9bd._pg_upgrade exactly for the new tables. The app role
    already exists (created by the head migration); here we only grant it DML on
    the new tables + sequence usage, then enable fail-closed RLS on the four
    tenant tables. scheduled_triggers gets DML grants but is intentionally NOT
    RLS-enabled (the bootstrap reads it without a workspace GUC).
    """
    for tbl in _ALL_TABLES:
        op.execute(f"GRANT SELECT, INSERT, UPDATE, DELETE ON {tbl} TO {APP_DB_ROLE}")
    # Serial PKs on trigger_action_results / trigger_cap_reservations need the sequences.
    op.execute(f"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO {APP_DB_ROLE}")
    # The on_signal emitter runs inside PgLeadStore.add_signal AS THE APP ROLE and
    # maps the signal's lead → its WorkbookRow(s). It therefore needs SELECT on the
    # (app-scoped, non-RLS) workbook tables. Read-only — writes still go through the
    # table-owner API connection. Idempotent if the grant already exists.
    for tbl in ("workbooks", "workbook_rows"):
        op.execute(f"GRANT SELECT ON {tbl} TO {APP_DB_ROLE}")

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


def downgrade() -> None:
    """Downgrade schema."""
    if op.get_bind().dialect.name == "postgresql":
        _pg_downgrade()

    op.drop_index("ix_scheduled_triggers_due", table_name="scheduled_triggers")
    op.drop_table("scheduled_triggers")
    op.drop_index("ix_capres_ws_day", table_name="trigger_cap_reservations")
    op.drop_index("ix_capres_ws_trig_day", table_name="trigger_cap_reservations")
    op.drop_table("trigger_cap_reservations")
    op.drop_index("ix_tar_ws_created", table_name="trigger_action_results")
    op.drop_index("ix_tar_ws_trigger_created", table_name="trigger_action_results")
    op.drop_table("trigger_action_results")
    op.drop_index("ix_trigger_runs_ws_trigger_started", table_name="trigger_runs")
    op.drop_index("ix_trigger_runs_ws", table_name="trigger_runs")
    op.drop_table("trigger_runs")
    op.drop_index("ix_triggers_ws_next_run", table_name="triggers")
    op.drop_index("ix_triggers_ws_enabled", table_name="triggers")
    op.drop_index("ix_triggers_ws_type", table_name="triggers")
    op.drop_index("ix_triggers_workspace_id", table_name="triggers")
    op.drop_table("triggers")


def _pg_downgrade() -> None:
    """Drop the RLS policies before the tables disappear. App role is not dropped
    (matches the head migration — it may be shared / granted to login roles)."""
    for tbl in _RLS_TABLES:
        op.execute(f"DROP POLICY IF EXISTS {tbl}_workspace_isolation ON {tbl}")
