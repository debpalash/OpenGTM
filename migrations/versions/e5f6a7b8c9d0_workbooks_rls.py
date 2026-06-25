"""workbooks: RLS-harden workbooks + child tables (denormalized workspace_id)

Revision ID: e5f6a7b8c9d0
Revises: d4e5f6a7b8c9
Create Date: 2026-06-26 12:00:00.000000

PR-B of the workbooks RLS-hardening (features/followup-workbooks-rls-hardening-
spec.md). PR-A already tenant-scoped every worker/request path while RLS was
still OFF; this revision flips on the same proven RLS posture as
leads/signals/outreach (c42d0273d9bd / b2c3d4e5f6a7):

  * the four child tables (workbook_rows, workbook_enrichments, workbook_activity,
    cell_traces) carry NO tenant column today — add a denormalized
    ``workspace_id``, backfilled from the parent workbook;
  * ``workbooks.workspace_id`` was nullable (legacy NULL rows) — backfill then
    tighten to NOT NULL;
  * NULL handling (OD-1a) = backfill-then-block: legacy NULL-workspace workbooks
    are attributed to the single workspace / the ``main`` workspace; if the data
    is ambiguous (multiple workspaces, no ``main``) the migration ABORTS rather
    than guess a tenant (a wrong guess is a cross-tenant leak);
  * Postgres-only: least-privilege GRANTs to the app role + ENABLE/FORCE ROW
    LEVEL SECURITY + a fail-closed USING/WITH CHECK policy on
    ``current_setting('app.workspace_id', true)`` for all five tables.

SQLite/self-host gets the columns + backfill + NOT NULL + indexes only (RLS DDL
is dialect-guarded). The order is critical: the backfill runs while the tables
are still policy-free, BEFORE ENABLE/FORCE (FORCE binds the policy to the owner
too, which would otherwise block the owner's own backfill writes).
"""
import os
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


# revision identifiers, used by Alembic.
revision: str = "e5f6a7b8c9d0"
down_revision: Union[str, Sequence[str], None] = "d4e5f6a7b8c9"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None

# Must match apps.api.core.config.settings.APP_DB_ROLE (and the head migrations).
APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")

# Child tables that gain a denormalized workspace_id this migration.
_CHILD_TABLES = (
    "workbook_rows",
    "workbook_enrichments",
    "workbook_activity",
    "cell_traces",
)
# All five tables that get RLS (parent + children).
_RLS_TABLES = ("workbooks",) + _CHILD_TABLES


# ── helpers ──────────────────────────────────────────────────────────────────

def _has_column(insp, table: str, col: str) -> bool:
    return col in {c["name"] for c in insp.get_columns(table)}


def _has_index(insp, table: str, name: str) -> bool:
    return name in {ix["name"] for ix in insp.get_indexes(table)}


def _resolve_fallback_workspace():
    """OD-1a: which workspace to attribute legacy NULL-workspace workbooks to.

    exactly-one-workspace → that workspace; else the workspace with slug 'main'
    (mirrors the old main.py startup backfill); else None → the caller aborts
    (never guess a tenant for ambiguous multi-tenant data).

    The workspaces live in a SEPARATE meta store (data/workspaces.db), not in
    this migration's bind, so resolve them via the workspace manager. Only called
    when NULL-workspace workbooks actually exist (a fresh DB has none, so this is
    never reached during the standard upgrade / test run).
    """
    from apps.api.services.workspace import manager as ws_manager

    wss = list(ws_manager.list_workspaces())
    if len(wss) == 1:
        return wss[0].id
    for ws in wss:
        if getattr(ws, "slug", None) == "main":
            return ws.id
    return None


# ── upgrade ──────────────────────────────────────────────────────────────────

def upgrade() -> None:
    bind = op.get_bind()
    insp = sa.inspect(bind)
    is_sqlite = bind.dialect.name == "sqlite"

    # 1) Add the denormalized workspace_id to each child table (nullable for now
    #    so existing rows survive; tightened to NOT NULL after the backfill).
    for tbl in _CHILD_TABLES:
        if not _has_column(insp, tbl, "workspace_id"):
            op.add_column(tbl, sa.Column("workspace_id", sa.String(), nullable=True))

    # 2a) Parent backfill (OD-1a) — replaces the removed main.py startup backfill.
    #     Runs while workbooks is still policy-free so the owner can write freely.
    null_wb = bind.execute(
        sa.text("SELECT count(*) FROM workbooks WHERE workspace_id IS NULL")
    ).scalar() or 0
    if null_wb:
        fallback = _resolve_fallback_workspace()
        if not fallback:
            raise RuntimeError(
                f"workbooks RLS migration aborted: {null_wb} workbook(s) have a "
                "NULL workspace_id and the target tenant is ambiguous (multiple "
                "workspaces exist with no 'main'). Assign each NULL workbook to "
                "its owning workspace before upgrading — never guess a tenant "
                "(a wrong guess is a cross-tenant leak). See OD-1a in "
                "features/followup-workbooks-rls-hardening-spec.md."
            )
        bind.execute(
            sa.text("UPDATE workbooks SET workspace_id = :ws WHERE workspace_id IS NULL"),
            {"ws": fallback},
        )

    # 2b) Child backfill from the parent workbook. Correlated subquery works on
    #     both PostgreSQL and SQLite (portable; orphan child rows whose
    #     workbook_id matches no workbook stay NULL and are caught in step 3).
    for tbl in _CHILD_TABLES:
        bind.execute(sa.text(
            f"UPDATE {tbl} SET workspace_id = "
            f"(SELECT w.workspace_id FROM workbooks w WHERE w.id = {tbl}.workbook_id) "
            f"WHERE workspace_id IS NULL"
        ))

    # 3) Guard against any remaining NULLs (orphans / unresolved) BEFORE NOT NULL.
    for tbl in _RLS_TABLES:
        remaining = bind.execute(
            sa.text(f"SELECT count(*) FROM {tbl} WHERE workspace_id IS NULL")
        ).scalar() or 0
        if remaining:
            raise RuntimeError(
                f"workbooks RLS migration aborted: {remaining} row(s) in '{tbl}' "
                "still have NULL workspace_id after backfill (likely orphaned "
                "rows with no parent workbook). Clean these up before upgrading."
            )

    # 4) Tighten every workspace_id to NOT NULL (parent + children). SQLite has
    #    no ALTER COLUMN, so use batch (table-recreate) there; PG alters in place.
    for tbl in _RLS_TABLES:
        if is_sqlite:
            with op.batch_alter_table(tbl) as batch_op:
                batch_op.alter_column(
                    "workspace_id", existing_type=sa.String(), nullable=False
                )
        else:
            op.alter_column(
                tbl, "workspace_id", existing_type=sa.String(), nullable=False
            )

    # 5) Index workspace_id on each child table (matches the model's index=True;
    #    keeps `alembic check` drift-free and serves the RLS policy lookup).
    #    workbooks already has ix_workbooks_workspace_id from the baseline.
    insp = sa.inspect(bind)  # refresh after the batch recreate
    for tbl in _CHILD_TABLES:
        ix_name = f"ix_{tbl}_workspace_id"
        if not _has_index(insp, tbl, ix_name):
            op.create_index(ix_name, tbl, ["workspace_id"], unique=False)

    # 6) Postgres-only: grants + ENABLE/FORCE RLS + fail-closed policies.
    if bind.dialect.name == "postgresql":
        _pg_upgrade()


def _pg_upgrade() -> None:
    """Least-privilege grants + RLS enable/force/policies on all five tables.

    The app role already exists (created by c42d0273d9bd); we only grant it DML
    on the workbook tables + sequence usage (serial PKs on rows/enrichments/
    activity/traces), then enable fail-closed RLS. Mirrors b2c3d4e5f6a7 exactly.
    """
    for tbl in _RLS_TABLES:
        op.execute(f"GRANT SELECT, INSERT, UPDATE, DELETE ON {tbl} TO {APP_DB_ROLE}")
    op.execute(f"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO {APP_DB_ROLE}")

    for tbl in _RLS_TABLES:
        op.execute(f"ALTER TABLE {tbl} ENABLE ROW LEVEL SECURITY")
        op.execute(f"ALTER TABLE {tbl} FORCE ROW LEVEL SECURITY")
        # DROP-then-CREATE so a partially-applied / re-run state is safe (PG has
        # no CREATE POLICY IF NOT EXISTS).
        op.execute(f"DROP POLICY IF EXISTS {tbl}_workspace_isolation ON {tbl}")
        op.execute(
            f"""
            CREATE POLICY {tbl}_workspace_isolation ON {tbl}
            USING (workspace_id = current_setting('app.workspace_id', true))
            WITH CHECK (workspace_id = current_setting('app.workspace_id', true))
            """
        )


# ── downgrade ────────────────────────────────────────────────────────────────

def downgrade() -> None:
    bind = op.get_bind()
    insp = sa.inspect(bind)
    is_sqlite = bind.dialect.name == "sqlite"

    if bind.dialect.name == "postgresql":
        _pg_downgrade()

    # Drop the child workspace_id indexes (PG drops them with the column too, but
    # be explicit/portable). Then drop the child columns and revert workbooks to
    # nullable. The app role is NOT dropped (matches the head migrations).
    for tbl in _CHILD_TABLES:
        ix_name = f"ix_{tbl}_workspace_id"
        if is_sqlite:
            with op.batch_alter_table(tbl) as batch_op:
                if _has_index(insp, tbl, ix_name):
                    batch_op.drop_index(ix_name)
                batch_op.drop_column("workspace_id")
        else:
            if _has_index(insp, tbl, ix_name):
                op.drop_index(ix_name, table_name=tbl)
            op.drop_column(tbl, "workspace_id")

    # workbooks.workspace_id reverts to nullable (column itself predates this rev).
    if is_sqlite:
        with op.batch_alter_table("workbooks") as batch_op:
            batch_op.alter_column(
                "workspace_id", existing_type=sa.String(), nullable=True
            )
    else:
        op.alter_column(
            "workbooks", "workspace_id", existing_type=sa.String(), nullable=True
        )


def _pg_downgrade() -> None:
    """Tear down the RLS policies + disable RLS before the columns are dropped."""
    for tbl in _RLS_TABLES:
        op.execute(f"DROP POLICY IF EXISTS {tbl}_workspace_isolation ON {tbl}")
        op.execute(f"ALTER TABLE {tbl} NO FORCE ROW LEVEL SECURITY")
        op.execute(f"ALTER TABLE {tbl} DISABLE ROW LEVEL SECURITY")
